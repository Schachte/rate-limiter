package ratelimiter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/go-redsync/redsync/v4"
	"github.com/google/uuid"
	"github.com/nitishm/go-rejson/v4/rjs"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

const (
	TestBucketCapacity = 1
	TestFillRate       = 5
	TestUnit           = time.Second
)

type MockRedisHandler struct {
	mu        sync.Mutex
	mockCache map[string][]byte
}

func (m *MockRedisHandler) JSONGet(key, path string, opts ...rjs.GetOption) (res interface{}, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, exists := m.mockCache[key]
	// simulate missing key
	if !exists {
		return nil, redis.Nil
	}
	return data, nil
}

func (m *MockRedisHandler) JSONSet(key string, path string, obj interface{}, opts ...rjs.SetOption) (res interface{}, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	jsonData, err := json.Marshal(obj)
	if err != nil {
		return nil, nil
	}
	m.mockCache[key] = jsonData
	return "OK", nil
}

type MockRedisMutexHandler struct {
	mu sync.Mutex
}

type MockRedisMutexLock struct {
	mu                    *sync.Mutex
	acquisitionShouldFail bool
	releaseShouldFail     bool
}

func (m *MockRedisMutexHandler) NewMutex(name string, options ...redsync.Option) MutexLock {
	return &MockRedisMutexLock{
		mu: &m.mu,
	}
}

func (m *MockRedisMutexLock) Lock() error {
	if m.acquisitionShouldFail {
		return errors.New("acquiring lock failed")
	}
	m.mu.Lock()
	return nil
}

func (m *MockRedisMutexLock) Unlock() (bool, error) {
	if m.releaseShouldFail {
		return false, errors.New("releasing lock failed")
	}
	m.mu.Unlock()
	return true, nil
}

func TestRateLimiter(t *testing.T) {
	// Our code is concurrent safe, so running unit tests in parallel
	// shouldn't be an issue and will improve overall build time of our
	// program.
	t.Parallel()

	t.Run("happy path new user is registered and comment is added", func(t *testing.T) {
		t.Parallel()
		mockCache := make(map[string][]byte)
		redisMutexHandler := MockRedisMutexHandler{}

		cfg := RateLimitConfig{
			RedisHostname: "localhost",
			RedisPort:     6379,
			ServerHost:    "0.0.0.0",
			ServerPort:    8080,

			FillRate:       TestFillRate,
			BucketCapacity: TestBucketCapacity,
			FillUnit:       TestUnit,

			RedisMu: &redisMutexHandler,
			RedisJSONHandler: &MockRedisHandler{
				mockCache: mockCache,
			},
		}

		rateLimiter, err := NewRateLimiter(cfg)
		require.NoError(t, err)

		handler, err := rateLimiter.RateLimitHandler()
		require.NoError(t, err)

		server := httptest.NewServer(http.HandlerFunc(handler))
		defer server.Close()

		uniqueUserIdentifier, err := uuid.NewRandom()
		require.NoError(t, err)

		newReq := Request{
			UserIdentifier: uniqueUserIdentifier.String(),
		}

		commentJSON, err := json.Marshal(newReq)
		if err != nil {
			t.Fatal(err)
		}

		req, err := http.NewRequest(
			http.MethodPost,
			fmt.Sprintf("%s/%s", server.URL, "limiter"),
			bytes.NewBuffer(commentJSON),
		)
		req.Header.Set("User-Identifier", uuid.NewString())
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		resp, err := server.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(
			t,
			http.StatusOK,
			resp.StatusCode,
			"status codes should match as StatusOK when rate limit not exceeded",
		)
	})

	t.Run("100 RPS should properly rate limit to the configured capacity", func(t *testing.T) {
		t.Parallel()
		mockCache := make(map[string][]byte)
		redisMutexHandler := MockRedisMutexHandler{}
		// 1 token per second
		currentFillRate := 1
		// 100 token volume
		currentCapacity := 100
		noConcurrentRequests := 100
		// cooldown before second batch of reqs
		coolDownPeriod := 10

		cfg := RateLimitConfig{
			RedisHostname: "localhost",
			RedisPort:     6379,
			ServerHost:    "0.0.0.0",
			ServerPort:    8080,

			FillRate:       time.Duration(currentFillRate),
			BucketCapacity: currentCapacity,
			FillUnit:       TestUnit,

			RedisMu: &redisMutexHandler,
			RedisJSONHandler: &MockRedisHandler{
				mockCache: mockCache,
			},
		}

		rateLimiter, err := NewRateLimiter(cfg)
		require.NoError(t, err)

		handler, err := rateLimiter.RateLimitHandler()
		require.NoError(t, err)

		server := httptest.NewServer(http.HandlerFunc(handler))
		defer server.Close()

		uniqueUserIdentifier, err := uuid.NewRandom()
		require.NoError(t, err)

		newReq := Request{
			UserIdentifier: uniqueUserIdentifier.String(),
		}

		reqJSON, err := json.Marshal(newReq)
		if err != nil {
			t.Fatal(err)
		}

		// store all the requests we want to invoke concurrently
		var wg sync.WaitGroup
		requestList := []*http.Request{}
		userIdentifier := uuid.NewString()
		for i := 0; i < noConcurrentRequests*2; i++ {
			req, err := http.NewRequest(
				http.MethodPost,
				fmt.Sprintf("%s/%s", server.URL, "limiter"),
				bytes.NewBuffer(reqJSON),
			)
			req.Header.Set("User-Identifier", userIdentifier)
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			requestList = append(requestList, req)
		}

		statusCodeAggregator := make(chan int, noConcurrentRequests*2)
		requestInvoker := func(req *http.Request, wg *sync.WaitGroup, statusCodeChan chan int) {
			defer wg.Done()
			resp, err := server.Client().Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			statusCodeAggregator <- resp.StatusCode
		}

		// invoke N no. of concurrent requests
		for i := 0; i < len(requestList)/2; i++ {
			wg.Add(1)
			go requestInvoker(requestList[i], &wg, statusCodeAggregator)
		}
		wg.Wait()

		time.Sleep(time.Duration(coolDownPeriod) * time.Second)

		// invoke an additional N no. of concurrent requests
		for i := len(requestList) / 2; i < len(requestList); i++ {
			wg.Add(1)
			go requestInvoker(requestList[i], &wg, statusCodeAggregator)
		}

		// wait for all concurrent requests to complete
		wg.Wait()
		close(statusCodeAggregator)
		successfullyProcessed := 0
		unsuccessfullyProcessed := 0
		for req := range statusCodeAggregator {
			if req == http.StatusOK {
				successfullyProcessed++
				continue
			}
			if req == http.StatusTooManyRequests {
				unsuccessfullyProcessed++
			}
		}

		require.Equal(t, currentCapacity+coolDownPeriod, successfullyProcessed)
		require.Equal(t, ((noConcurrentRequests * 2) - (currentCapacity + coolDownPeriod)), unsuccessfullyProcessed)
	})

	t.Run("rate limits apply and reject when token bucket is empty", func(t *testing.T) {
		t.Parallel()
		mockCache := make(map[string][]byte)
		redisMutexHandler := MockRedisMutexHandler{}

		cfg := RateLimitConfig{
			RedisHostname: "localhost",
			RedisPort:     6379,
			ServerHost:    "0.0.0.0",
			ServerPort:    8080,

			FillRate:       TestFillRate,
			BucketCapacity: TestBucketCapacity,
			FillUnit:       TestUnit,

			RedisMu: &redisMutexHandler,
			RedisJSONHandler: &MockRedisHandler{
				mockCache: mockCache,
			},
		}

		rateLimiter, err := NewRateLimiter(cfg)
		require.NoError(t, err)

		handler, err := rateLimiter.RateLimitHandler()
		require.NoError(t, err)

		server := httptest.NewServer(http.HandlerFunc(handler))
		defer server.Close()

		uniqueUserIdentifier, err := uuid.NewRandom()
		require.NoError(t, err)

		newReq := Request{
			UserIdentifier: uniqueUserIdentifier.String(),
		}

		reqJSON, err := json.Marshal(newReq)
		if err != nil {
			t.Fatal(err)
		}

		req, err := http.NewRequest(
			http.MethodPost,
			fmt.Sprintf("%s/%s", server.URL, "limiter"),
			bytes.NewBuffer(reqJSON),
		)
		require.NoError(t, err)
		req.Header.Set("User-Identifier", uuid.NewString())
		req.Header.Set("Content-Type", "application/json")

		resp, err := server.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(
			t,
			http.StatusOK,
			resp.StatusCode,
			"status codes should match as StatusOK when rate limit not exceeded",
		)

		// immediately invoke another request which would violate the rate limit
		// policy
		req, err = http.NewRequest(
			http.MethodPost,
			fmt.Sprintf("%s/%s", server.URL, "limiter"),
			bytes.NewBuffer(reqJSON),
		)
		req.Header.Set("User-Identifier", uuid.NewString())
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err = server.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		// we expect an error on the response as the rate limit should be exceeded
		_, err = ioutil.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)

		// wait until refill rate can add a token
		time.Sleep(TestUnit * TestFillRate)

		req, err = http.NewRequest(
			http.MethodPost,
			fmt.Sprintf("%s/%s", server.URL, "limiter"),
			bytes.NewBuffer(reqJSON),
		)
		req.Header.Set("User-Identifier", uuid.NewString())
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err = server.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("concurrent requests made from the same user can't bypass rate limits", func(t *testing.T) {
		t.Parallel()
		bucketCapacity := 7
		noConcurrentRequests := 100

		mockCache := make(map[string][]byte)
		redisMutexHandler := MockRedisMutexHandler{}

		cfg := RateLimitConfig{
			RedisHostname: "localhost",
			RedisPort:     6379,
			ServerHost:    "0.0.0.0",
			ServerPort:    8080,

			FillRate:       15,
			BucketCapacity: bucketCapacity,
			FillUnit:       TestUnit,

			RedisMu: &redisMutexHandler,
			RedisJSONHandler: &MockRedisHandler{
				mockCache: mockCache,
			},
		}

		rateLimiter, err := NewRateLimiter(cfg)
		require.NoError(t, err)

		handler, err := rateLimiter.RateLimitHandler()
		require.NoError(t, err)

		server := httptest.NewServer(http.HandlerFunc(handler))
		defer server.Close()

		uniqueUserIdentifier, err := uuid.NewRandom()
		require.NoError(t, err)

		newReq := Request{
			UserIdentifier: uniqueUserIdentifier.String(),
		}

		commentJSON, err := json.Marshal(newReq)
		if err != nil {
			t.Fatal(err)
		}

		// store all the requests we want to invoke concurrently
		var wg sync.WaitGroup
		requestList := []*http.Request{}
		userIdentifier := uuid.NewString()
		for i := 0; i < noConcurrentRequests; i++ {
			req, err := http.NewRequest(
				http.MethodPost,
				fmt.Sprintf("%s/%s", server.URL, "limiter"),
				bytes.NewBuffer(commentJSON),
			)
			req.Header.Set("User-Identifier", userIdentifier)
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			requestList = append(requestList, req)
		}

		statusCodeAggregator := make(chan int, noConcurrentRequests)
		requestInvoker := func(req *http.Request, wg *sync.WaitGroup, statusCodeChan chan int) {
			defer wg.Done()
			resp, err := server.Client().Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			statusCodeAggregator <- resp.StatusCode
		}

		for _, currentRequest := range requestList {
			wg.Add(1)
			go requestInvoker(currentRequest, &wg, statusCodeAggregator)
		}

		// wait for all concurrent requests to complete
		wg.Wait()
		close(statusCodeAggregator)
		successfullyProcessed := 0
		for req := range statusCodeAggregator {
			if req == http.StatusOK {
				successfullyProcessed++
			}
		}

		require.Equal(t, bucketCapacity, successfullyProcessed)
	})

	t.Run("missing User-Identifier on web proxy fails request", func(t *testing.T) {
		t.Parallel()
		mockCache := make(map[string][]byte)
		redisMutexHandler := MockRedisMutexHandler{}

		cfg := RateLimitConfig{
			RedisHostname: "localhost",
			RedisPort:     6379,
			ServerHost:    "0.0.0.0",
			ServerPort:    8080,

			FillRate:       TestFillRate,
			BucketCapacity: TestBucketCapacity,
			FillUnit:       TestUnit,

			RedisMu: &redisMutexHandler,
			RedisJSONHandler: &MockRedisHandler{
				mockCache: mockCache,
			},
		}

		rateLimiter, err := NewRateLimiter(cfg)
		require.NoError(t, err)

		handler, err := rateLimiter.RateLimitHandler()
		require.NoError(t, err)

		server := httptest.NewServer(http.HandlerFunc(handler))
		defer server.Close()

		uniqueUserIdentifier, err := uuid.NewRandom()
		require.NoError(t, err)

		newReq := Request{
			UserIdentifier: uniqueUserIdentifier.String(),
		}

		reqJSON, err := json.Marshal(newReq)
		if err != nil {
			t.Fatal(err)
		}

		req, err := http.NewRequest(
			http.MethodPost,
			fmt.Sprintf("%s/%s", server.URL, "limiter"),
			bytes.NewBuffer(reqJSON),
		)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		resp, err := server.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, MissingIdentifierHeader, string(respBody[0:len(respBody)-1]))
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
}

func TestRequestEvaluator(t *testing.T) {
	t.Parallel()

	mockCache := make(map[string][]byte)
	redisMutexHandler := MockRedisMutexHandler{}
	cfg := RateLimitConfig{
		RedisHostname: "localhost",
		RedisPort:     6379,
		ServerHost:    "0.0.0.0",
		ServerPort:    8080,

		FillRate:       TestFillRate,
		BucketCapacity: TestBucketCapacity,
		FillUnit:       TestUnit,

		RedisMu: &redisMutexHandler,
		RedisJSONHandler: &MockRedisHandler{
			mockCache: mockCache,
		},
	}

	t.Run("fails when user identifier is missing", func(t *testing.T) {
		t.Parallel()

		rateLimiter, err := NewRateLimiter(cfg)
		require.NoError(t, err)

		err = rateLimiter.EvaluateRequest(Request{})
		require.Error(t, err, MissingIdentifierHeader)
	})

	t.Run("fails when user identifier is missing", func(t *testing.T) {
		t.Parallel()

		rateLimiter, err := NewRateLimiter(cfg)
		require.NoError(t, err)

		err = rateLimiter.EvaluateRequest(Request{})
		require.Error(t, err, MissingIdentifierHeader)
	})

	t.Run("fails when lock can't be acquired", func(t *testing.T) {
		t.Parallel()

		rateLimiter, err := NewRateLimiter(cfg)
		require.NoError(t, err)

		err = rateLimiter.EvaluateRequest(Request{})
		require.Error(t, err, MissingIdentifierHeader)
	})
}
