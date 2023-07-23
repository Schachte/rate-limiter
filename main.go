package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

const (
	// StandardBucketCapacity is the volume of the bucket per-user representing how many
	// tokens will fit in the bucket
	StandardBucketCapacity = 1
	// StandardFillRate is the token refill rate in conjunction with the StandardUnit
	StandardFillRate = 30
	// StandardUnit is the unit used to determine cadence of the token refill
	StandardUnit = time.Second
)

// CommentHandlerConfig contains all the information used to dynamically
// configure our handler with different rates and storage providers
type CommentHandlerConfig struct {
	CommentStore   *Comments
	BucketMap      map[string]*Bucket
	BucketCapacity int
	FillRate       time.Duration
	FillUnit       time.Duration
}

// Comments simulates our persistence layer by storing all comments
// across all of the articles we have.
type Comments struct {
	mu       sync.Mutex
	comments []Comment
}

type Comment struct {
	UserIdentifier string `json:"UserIdentifier"`
	// ArticleId will track which unique article the comment should be assigned to
	ArticleId int `json:"ArticleID"`
	// Name is the optional name a user can append to their comment
	Name string `json:"Name"`
	// DateAdded adds a time component to track when a comment was added
	DateAdded time.Time `json:"DateAdded"`
	// Comment is the contents of the user-added comment
	Comment string `json:"Comment"`
}

// Bucket is the abstraction used to rate limit users on a per-user level
type Bucket struct {
	// userIdentifier is the unique identification value used to track usage on a per-customer basis
	userIdentifier string
	// tokens is the capacity of a given bucket (number of tokens it's capable of holding)
	tokens int
	// fillRate is the fixed rate in which we add tokens to the bucket unless or until it's at capacity
	fillRate time.Duration
	// unit is the unit of measurement in which we fill the bucket (seconds, minutes, hours)
	unit time.Duration
	// lastChecked is the last recorded timestep in which we filled the bucket
	lastChecked time.Time
	// mu allows us to lock the mutation of a buckets tokens as it's a critical
	// section of code that be mutated by multiple threads
	mu sync.Mutex
}

// handleComment will evaluate each comment on a per-user basis and determine if the comment should
// be persisted or not based on the rate limiting policy defined by the system.
func handleComment(config *CommentHandlerConfig) (func(w http.ResponseWriter, r *http.Request), error) {
	if config == nil {
		return nil, errors.New("missing handler configuration")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var newComment Comment
			err := json.NewDecoder(r.Body).Decode(&newComment)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}

			// obtain the user to evaluate
			userBucket, exists := config.BucketMap[newComment.UserIdentifier]
			if !exists {
				userBucket = &Bucket{
					userIdentifier: newComment.UserIdentifier,
					fillRate:       config.FillRate,
					tokens:         config.BucketCapacity,
					unit:           config.FillUnit,
				}
				config.BucketMap[newComment.UserIdentifier] = userBucket
			}

			err = userBucket.verifyCommentAllowance()
			if err != nil {
				http.Error(w, "Rate limit exceeded, try again later", http.StatusTooManyRequests)
				return
			}

			newComment = config.CommentStore.addComment(newComment)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(newComment)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	}, nil
}

func main() {
	// In-memory map to track individual users and their rates and limits
	userRateLimitTracker := make(map[string]*Bucket)
	handlerConfiguration := CommentHandlerConfig{
		BucketMap:      userRateLimitTracker,
		CommentStore:   &Comments{},
		FillRate:       StandardFillRate,
		BucketCapacity: StandardBucketCapacity,
		FillUnit:       StandardUnit,
	}
	commentHandler, err := handleComment(&handlerConfiguration)
	if err != nil {
		log.Fatal("unable to initialize handler with missing configuration")
	}

	http.HandleFunc("/comments", commentHandler)
	http.ListenAndServe(":8080", nil)
}

// verifyCommentAllowance will employ the token bucket algorithm which
// will reference a specific users usage quota to determine if the comment
// can be added or not.
func (b *Bucket) verifyCommentAllowance() error {
	// lock the bucket during evaluation of state
	b.mu.Lock()
	// avoid deadlock by auto unlocking the mutex when function execution
	// completes
	defer b.mu.Unlock()
	currentTime := time.Now()

	// if the number of tokens is non-empty, we know the request is
	// ok to process. From here, we can drain a token and update
	// the evaluation time.
	if b.tokens > 0 {
		b.tokens--
		b.lastChecked = currentTime
		return nil
	}

	// threshold calculates a delta between now and whatever our fill rate is in the past
	// to evaluate if we're able to process the request or not.
	threshold := currentTime.Add(-1 * b.fillRate * b.unit)

	// If the current time is 11:00AM and the lastChecked time is 10:59AM
	// we know that we can only add a token and process the request if
	// the lastChecked time happened before the threshold
	if b.lastChecked.Before(threshold) {
		b.lastChecked = currentTime
		return nil
	}

	return fmt.Errorf("unable to process comment as last added comment was: %v and current time is: %v",
		b.lastChecked,
		currentTime,
	)
}

// addComment will add a comment if the users rate limit hasn't been enforced
func (c *Comments) addComment(comment Comment) Comment {
	c.mu.Lock()
	defer c.mu.Unlock()
	comment.DateAdded = time.Now()
	c.comments = append(c.comments, comment)
	return comment
}
