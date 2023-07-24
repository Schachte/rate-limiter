package main

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	mu *sync.Mutex
}

func (m *MockRedisMutexHandler) NewMutex(name string, options ...redsync.Option) MutexLock {
	return &MockRedisMutexLock{
		mu: &m.mu,
	}
}

func (m *MockRedisMutexLock) Lock() error {
	m.mu.Lock()
	return nil
}

func (m *MockRedisMutexLock) Unlock() (bool, error) {
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
		handlerConfiguration := RateLimitConfig{
			RedisJSONHandler: &MockRedisHandler{
				mockCache: mockCache,
			},
			RedisMu:        &redisMutexHandler,
			FillRate:       TestFillRate,
			BucketCapacity: TestBucketCapacity,
			FillUnit:       TestUnit,
		}
		commentHandler, err := rateLimitHandler(&handlerConfiguration)
		require.NoError(t, err)

		server := httptest.NewServer(http.HandlerFunc(commentHandler))
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

	t.Run("rate limits apply and reject when token bucket is empty", func(t *testing.T) {
		t.Parallel()
		mockCache := make(map[string][]byte)
		redisMutexHandler := MockRedisMutexHandler{}
		handlerConfiguration := RateLimitConfig{
			RedisJSONHandler: &MockRedisHandler{
				mockCache: mockCache,
			},
			RedisMu:        &redisMutexHandler,
			FillRate:       TestFillRate,
			BucketCapacity: TestBucketCapacity,
			FillUnit:       TestUnit,
		}
		commentHandler, err := rateLimitHandler(&handlerConfiguration)
		require.NoError(t, err)

		server := httptest.NewServer(http.HandlerFunc(commentHandler))
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

		// immediately invoke another request which would violate the rate limit
		// policy
		req, err = http.NewRequest(
			http.MethodPost,
			fmt.Sprintf("%s/%s", server.URL, "limiter"),
			bytes.NewBuffer(commentJSON),
		)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err = server.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		// we expect an error on the response as the rate limit should be exceeded
		responseBody, err := ioutil.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
		require.Equal(t, fmt.Sprintf("%s\n", RateLimitExceeded), string(responseBody))

		// wait until refill rate can add a token
		time.Sleep(TestUnit * TestFillRate)

		req, err = http.NewRequest(
			http.MethodPost,
			fmt.Sprintf("%s/%s", server.URL, "limiter"),
			bytes.NewBuffer(commentJSON),
		)
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
		noConcurrentRequests := 120

		mockCache := make(map[string][]byte)
		redisMutexHandler := MockRedisMutexHandler{}
		handlerConfiguration := RateLimitConfig{
			RedisJSONHandler: &MockRedisHandler{
				mockCache: mockCache,
			},
			RedisMu:        &redisMutexHandler,
			FillRate:       10,
			BucketCapacity: bucketCapacity,
			FillUnit:       TestUnit,
		}
		commentHandler, err := rateLimitHandler(&handlerConfiguration)
		require.NoError(t, err)

		server := httptest.NewServer(http.HandlerFunc(commentHandler))
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
		for i := 0; i < noConcurrentRequests; i++ {
			req, err := http.NewRequest(
				http.MethodPost,
				fmt.Sprintf("%s/%s", server.URL, "limiter"),
				bytes.NewBuffer(commentJSON),
			)
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
}
