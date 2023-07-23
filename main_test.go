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

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const (
	TestBucketCapacity = 1
	TestFillRate       = 5
	TestUnit           = time.Second
)

func TestRateLimiter(t *testing.T) {
	// Our code is concurrent safe, so running unit tests in parallel
	// shouldn't be an issue and will improve overall build time of our
	// program.
	t.Parallel()

	t.Run("happy path new user is registered and comment is added", func(t *testing.T) {
		t.Parallel()
		var usersRateLimitTracker = make(map[string]*Bucket)
		handlerConfiguration := CommentHandlerConfig{
			BucketMap:      usersRateLimitTracker,
			FillRate:       TestFillRate,
			BucketCapacity: TestBucketCapacity,
			FillUnit:       TestUnit,
			CommentStore:   &Comments{},
		}
		commentHandler, err := handleComment(&handlerConfiguration)
		require.NoError(t, err)

		server := httptest.NewServer(http.HandlerFunc(commentHandler))
		defer server.Close()

		uniqueUserIdentifier, err := uuid.NewRandom()
		require.NoError(t, err)

		newComment := Comment{
			UserIdentifier: uniqueUserIdentifier.String(),
			ArticleId:      1,
			Name:           "Ryan S.",
			Comment:        "Testing this blog system",
		}

		commentJSON, err := json.Marshal(newComment)
		if err != nil {
			t.Fatal(err)
		}

		req, err := http.NewRequest(
			http.MethodPost,
			fmt.Sprintf("%s/%s", server.URL, "comments"),
			bytes.NewBuffer(commentJSON),
		)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		resp, err := server.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(
			t,
			http.StatusCreated,
			resp.StatusCode,
			"status codes should match as StatusCreated when rate limit not exceeded",
		)

		var createdComment Comment
		err = json.NewDecoder(resp.Body).Decode(&createdComment)
		require.NoError(t, err)

		// Ensure comment created successfully
		require.Equal(t, newComment.UserIdentifier, createdComment.UserIdentifier)
		require.Equal(t, newComment.Comment, createdComment.Comment)

		// Verify user added to user tracker store properly
		require.Contains(t, usersRateLimitTracker, uniqueUserIdentifier.String())
	})

	t.Run("rate limits apply and reject when token bucket is empty", func(t *testing.T) {
		t.Parallel()
		var usersRateLimitTracker = make(map[string]*Bucket)
		handlerConfiguration := CommentHandlerConfig{
			BucketMap:      usersRateLimitTracker,
			FillRate:       TestFillRate,
			BucketCapacity: TestBucketCapacity,
			FillUnit:       TestUnit,
			CommentStore:   &Comments{},
		}
		commentHandler, err := handleComment(&handlerConfiguration)
		require.NoError(t, err)

		server := httptest.NewServer(http.HandlerFunc(commentHandler))
		defer server.Close()

		uniqueUserIdentifier, err := uuid.NewRandom()
		require.NoError(t, err)

		newComment := Comment{
			UserIdentifier: uniqueUserIdentifier.String(),
			ArticleId:      1,
			Name:           "Ryan S.",
			Comment:        "Testing this blog system",
		}

		commentJSON, err := json.Marshal(newComment)
		if err != nil {
			t.Fatal(err)
		}

		req, err := http.NewRequest(
			http.MethodPost,
			fmt.Sprintf("%s/%s", server.URL, "comments"),
			bytes.NewBuffer(commentJSON),
		)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		resp, err := server.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		require.Equal(
			t,
			http.StatusCreated,
			resp.StatusCode,
			"status codes should match as StatusCreated when rate limit not exceeded",
		)

		// immediately invoke another request which would violate the rate limit
		// policy
		req, err = http.NewRequest(
			http.MethodPost,
			fmt.Sprintf("%s/%s", server.URL, "comments"),
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
		require.Equal(t, "Rate limit exceeded, try again later\n", string(responseBody))

		// wait until refill rate can add a token
		time.Sleep(TestUnit * TestFillRate)

		req, err = http.NewRequest(
			http.MethodPost,
			fmt.Sprintf("%s/%s", server.URL, "comments"),
			bytes.NewBuffer(commentJSON),
		)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err = server.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusCreated, resp.StatusCode)
	})

	t.Run("concurrent requests made from the same user can't bypass rate limits", func(t *testing.T) {
		t.Parallel()
		bucketCapacity := 5
		noConcurrentRequests := 10

		var usersRateLimitTracker = make(map[string]*Bucket)
		handlerConfiguration := CommentHandlerConfig{
			BucketMap:      usersRateLimitTracker,
			FillRate:       TestFillRate,
			BucketCapacity: bucketCapacity,
			FillUnit:       TestUnit,
			CommentStore:   &Comments{},
		}
		commentHandler, err := handleComment(&handlerConfiguration)
		require.NoError(t, err)

		server := httptest.NewServer(http.HandlerFunc(commentHandler))
		defer server.Close()

		uniqueUserIdentifier, err := uuid.NewRandom()
		require.NoError(t, err)

		newComment := Comment{
			UserIdentifier: uniqueUserIdentifier.String(),
			ArticleId:      1,
			Name:           "Ryan S.",
			Comment:        "Testing this blog system",
		}

		commentJSON, err := json.Marshal(newComment)
		if err != nil {
			t.Fatal(err)
		}

		// store all the requests we want to invoke concurrently
		var wg sync.WaitGroup
		requestList := []*http.Request{}
		for i := 0; i < noConcurrentRequests; i++ {
			req, err := http.NewRequest(
				http.MethodPost,
				fmt.Sprintf("%s/%s", server.URL, "comments"),
				bytes.NewBuffer(commentJSON),
			)
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			requestList = append(requestList, req)
		}

		requestInvoker := func(req *http.Request, wg *sync.WaitGroup) {
			defer wg.Done()
			resp, err := server.Client().Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
		}

		for _, currentRequest := range requestList {
			wg.Add(1)
			go requestInvoker(currentRequest, &wg)
		}

		// wait for all concurrent requests to complete
		wg.Wait()
		require.Len(t, handlerConfiguration.CommentStore.comments, bucketCapacity)
	})
}
