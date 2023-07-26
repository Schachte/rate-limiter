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
	"go.uber.org/zap"
)

const (
	TestBucketCapacity = 1
	TestFillRate       = 5
	TestUnit           = time.Second
)

type MockRedisHandler struct {
	mu        sync.Mutex
	mockCache map[string][]byte

	shouldFailOnGet bool
	shouldFailOnSet bool

	shouldFailOnSetInvocation int
	shouldFailOnGetInvocation int

	getInvocationCount int
	setInvocationCount int

	shouldSetInvalidBytes bool
}

func (m *MockRedisHandler) JSONGet(key, path string, opts ...rjs.GetOption) (res interface{}, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shouldFailOnGet && m.shouldFailOnGetInvocation == m.getInvocationCount {
		return nil, NewUnableToGetJSONKey().WithError(errors.New("can't get key"))
	}
	m.getInvocationCount++
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
	if m.shouldFailOnSet && m.shouldFailOnSetInvocation == m.setInvocationCount {
		return nil, NewUnableToSetJSONKey().WithError(errors.New("can't set key"))
	}
	m.setInvocationCount++

	// simulate bad data to mock deserialization failure
	if m.shouldSetInvalidBytes {
		m.mockCache[key] = []byte{}
		return "OK", nil
	}

	jsonData, err := json.Marshal(obj)
	if err != nil {
		return nil, nil
	}
	m.mockCache[key] = jsonData
	return "OK", nil
}

type MockRedisMutexHandler struct {
	mu                    sync.Mutex
	acquisitionShouldFail bool
	releaseShouldFail     bool
}

type MockRedisMutexLock struct {
	mu                    *sync.Mutex
	acquisitionShouldFail bool
	releaseShouldFail     bool
}

func (m *MockRedisMutexHandler) NewMutex(name string, options ...redsync.Option) MutexLock {
	lock := &MockRedisMutexLock{
		mu: &m.mu,
	}
	if m.acquisitionShouldFail {
		lock.acquisitionShouldFail = m.acquisitionShouldFail
	}
	if m.releaseShouldFail {
		lock.releaseShouldFail = m.releaseShouldFail
	}
	return lock
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
	logger, err := zap.NewProduction()
	if err != nil {
		require.NoError(t, err)
	}
	defer logger.Sync()

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
		rateLimiter := RateLimiter{
			cfg,
			logger,
		}

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

	t.Run("internal cache failure propagates error to client", func(t *testing.T) {
		t.Parallel()
		mockCache := make(map[string][]byte)
		redisMutexHandler := MockRedisMutexHandler{
			acquisitionShouldFail: true,
		}

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
		rateLimiter := RateLimiter{
			cfg,
			logger,
		}

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
		req.Header.Set("User-Identifier", uniqueUserIdentifier.String())
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		resp, err := server.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(
			t,
			http.StatusInternalServerError,
			resp.StatusCode,
		)
	})

	t.Run("non-POST req fails against HTTP server", func(t *testing.T) {
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
		rateLimiter := RateLimiter{
			cfg,
			logger,
		}

		handler, err := rateLimiter.RateLimitHandler()
		require.NoError(t, err)

		server := httptest.NewServer(http.HandlerFunc(handler))
		defer server.Close()

		req, err := http.NewRequest(
			http.MethodGet,
			fmt.Sprintf("%s/%s", server.URL, "limiter"),
			bytes.NewBuffer([]byte{}),
		)
		req.Header.Set("User-Identifier", uuid.NewString())
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		resp, err := server.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(
			t,
			http.StatusMethodNotAllowed,
			resp.StatusCode,
		)
	})

	t.Run("initial request deserialization fails", func(t *testing.T) {
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
		rateLimiter := RateLimiter{
			cfg,
			logger,
		}

		handler, err := rateLimiter.RateLimitHandler()
		require.NoError(t, err)

		server := httptest.NewServer(http.HandlerFunc(handler))
		defer server.Close()

		req, err := http.NewRequest(
			http.MethodPost,
			fmt.Sprintf("%s/%s", server.URL, "limiter"),
			bytes.NewBuffer([]byte{}),
		)
		req.Header.Set("User-Identifier", uuid.NewString())
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		resp, err := server.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(
			t,
			http.StatusBadRequest,
			resp.StatusCode,
		)
	})

	t.Run("excess tokens exceeding bucket capacity overflow and don't persist", func(t *testing.T) {
		t.Parallel()
		mockCache := make(map[string][]byte)
		redisMutexHandler := MockRedisMutexHandler{}

		cfg := RateLimitConfig{
			RedisHostname: "localhost",
			RedisPort:     6379,
			ServerHost:    "0.0.0.0",
			ServerPort:    8080,

			// add a new token every second with a capacity of 1
			FillRate:       1,
			BucketCapacity: 1,
			FillUnit:       TestUnit,

			RedisMu: &redisMutexHandler,
			RedisJSONHandler: &MockRedisHandler{
				mockCache: mockCache,
			},
		}
		rateLimiter := RateLimiter{
			cfg,
			logger,
		}

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
		req.Header.Set("User-Identifier", newReq.UserIdentifier)
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

		// wait for tokens to fill up and exceed capacity (5 seconds = 5 tokens)
		time.Sleep(5 * time.Second)

		req, err = http.NewRequest(
			http.MethodPost,
			fmt.Sprintf("%s/%s", server.URL, "limiter"),
			bytes.NewBuffer(commentJSON),
		)
		req.Header.Set("User-Identifier", newReq.UserIdentifier)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		resp, err = server.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(
			t,
			http.StatusOK,
			resp.StatusCode,
			"status codes should match as StatusOK when rate limit not exceeded",
		)
		addedUser := &Bucket{}
		err = json.Unmarshal(mockCache[newReq.UserIdentifier], addedUser)
		require.NoError(t, err)
		require.LessOrEqual(t, addedUser.Capacity, cfg.BucketCapacity)
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

		rateLimiter := RateLimiter{
			cfg,
			logger,
		}

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

		rateLimiter := RateLimiter{
			cfg,
			logger,
		}

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
		req.Header.Set("User-Identifier", newReq.UserIdentifier)
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
		req.Header.Set("User-Identifier", newReq.UserIdentifier)
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
		req.Header.Set("User-Identifier", newReq.UserIdentifier)
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

		rateLimiter := RateLimiter{
			cfg,
			logger,
		}

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

		rateLimiter := RateLimiter{
			cfg,
			logger,
		}

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
	logger, err := zap.NewProduction()
	if err != nil {
		require.NoError(t, err)
	}
	defer logger.Sync()

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

		rateLimiter := RateLimiter{
			cfg,
			logger,
		}
		err = rateLimiter.EvaluateRequest(Request{})
		require.Error(t, err, MissingIdentifierHeader)
	})

	t.Run("fails when lock can't be acquired", func(t *testing.T) {
		t.Parallel()

		mockCache := make(map[string][]byte)
		redisMutexHandler := MockRedisMutexHandler{
			acquisitionShouldFail: true,
		}
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
		rateLimiter := RateLimiter{
			cfg,
			logger,
		}
		err = rateLimiter.EvaluateRequest(Request{UserIdentifier: "test-user"})
		require.Error(t, err, UnableToAcquireLock)
	})

	t.Run("fails when lock can't be released", func(t *testing.T) {
		t.Parallel()

		mockCache := make(map[string][]byte)
		redisMutexHandler := MockRedisMutexHandler{
			releaseShouldFail: true,
		}
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
		rateLimiter := RateLimiter{
			cfg,
			logger,
		}
		err = rateLimiter.EvaluateRequest(Request{UserIdentifier: "test-user"})
		require.Error(t, err, UnableToReleaseLock)
	})

	t.Run("fails when JSON key can't set", func(t *testing.T) {
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
				mockCache:       mockCache,
				shouldFailOnSet: true,
			},
		}
		rateLimiter := RateLimiter{
			cfg,
			logger,
		}
		err = rateLimiter.EvaluateRequest(Request{UserIdentifier: "test-user"})
		require.Error(t, err, UnableToSetJSONKey)
	})

	t.Run("fails when JSON key can't set final update", func(t *testing.T) {
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
				mockCache:                 mockCache,
				shouldFailOnSet:           true,
				shouldFailOnSetInvocation: 1,
			},
		}
		rateLimiter := RateLimiter{
			cfg,
			logger,
		}
		err = rateLimiter.EvaluateRequest(Request{UserIdentifier: "test-user"})
		require.Error(t, err, UnableToSetJSONKey)
	})

	t.Run("fails when JSON key can't get", func(t *testing.T) {
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
				mockCache:       mockCache,
				shouldFailOnGet: true,
			},
		}
		rateLimiter := RateLimiter{
			cfg,
			logger,
		}
		err = rateLimiter.EvaluateRequest(Request{UserIdentifier: "test-user"})
		require.Error(t, err, UnableToGetJSONKey)
	})

	t.Run("fails when JSON value can't get be deserialized from Redis", func(t *testing.T) {
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
				mockCache:             mockCache,
				shouldSetInvalidBytes: true,
			},
		}
		rateLimiter := RateLimiter{
			cfg,
			logger,
		}
		// First attempt succeeds and persists invalid value into cache
		err = rateLimiter.EvaluateRequest(Request{UserIdentifier: "test-user"})
		require.Nil(t, err)

		// Second lookup fails on existing user due to deserialization
		err = rateLimiter.EvaluateRequest(Request{UserIdentifier: "test-user"})
		require.Error(t, err, UnableToDeserialize)
	})
}
