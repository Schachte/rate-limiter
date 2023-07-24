package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/go-redsync/redsync/v4"
	"github.com/go-redsync/redsync/v4/redis/goredis/v9"
	"github.com/gomodule/redigo/redis"
	goredislib "github.com/redis/go-redis/v9"

	"github.com/nitishm/go-rejson/v4"
	"github.com/nitishm/go-rejson/v4/rjs"
)

const (
	UnableToProcess   = "unable to process request, please try again"
	RateLimitExceeded = "rate limit exceeded, try again later"
	MethodNotAllowed  = "method not allowed"
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

type RedisStore interface {
	JSONGet(key, path string, opts ...rjs.GetOption) (res interface{}, err error)
	JSONSet(key string, path string, obj interface{}, opts ...rjs.SetOption) (res interface{}, err error)
}

type RedisMutex interface {
	NewMutex(name string, options ...redsync.Option) MutexLock
}

type MutexLock interface {
	Lock() error
	Unlock() (bool, error)
}

type RedisMutexHandler struct {
	redSync *redsync.Redsync
}

type RedisMutexLock struct {
	lock redsync.Mutex
}

func (m *RedisMutexHandler) NewMutex(name string, options ...redsync.Option) MutexLock {
	return m.redSync.NewMutex(name)
}

func (m *RedisMutexLock) Lock() error {
	return m.lock.Lock()
}

func (m *RedisMutexLock) Unlock() (bool, error) {
	return m.lock.Unlock()
}

// CommentHandlerConfig contains all the information used to dynamically
// configure our handler with different rates and storage providers
type CommentHandlerConfig struct {
	RedisMu          RedisMutex
	RedisJSONHandler RedisStore
	CommentStore     *Comments
	BucketCapacity   int
	FillRate         time.Duration
	FillUnit         time.Duration
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
	UserIdentifier string `json:"UserIdentifier"`
	// tokens is the capacity of a given bucket (number of tokens it's capable of holding)
	Tokens int `json:"Tokens"`
	// fillRate is the fixed rate in which we add tokens to the bucket unless or until it's at capacity
	FillRate time.Duration `json:"FillRate"`
	// unit is the unit of measurement in which we fill the bucket (seconds, minutes, hours)
	Unit time.Duration `json:"Unit"`
	// lastChecked is the last recorded timestep in which we filled the bucket
	LastChecked time.Time `json:"LastChecked"`
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

			mtx := config.RedisMu.NewMutex(fmt.Sprintf("%s-lock", newComment.UserIdentifier))
			if err := mtx.Lock(); err != nil {
				panic(err)
			}
			defer func() {
				if ok, err := mtx.Unlock(); !ok || err != nil {
					panic("unlock failed")
				}
			}()
			userBucket := &Bucket{}
			jsonData, err := config.RedisJSONHandler.JSONGet(newComment.UserIdentifier, ".")
			if err != nil {
				if err != goredislib.Nil {
					http.Error(w, UnableToProcess, http.StatusInternalServerError)
					return
				}
				// in this scenario, we just need to store the new user that we haven't processed before
				userBucket = &Bucket{
					UserIdentifier: newComment.UserIdentifier,
					FillRate:       config.FillRate,
					Tokens:         config.BucketCapacity,
					Unit:           config.FillUnit,
				}

				// update the users bucket state after evaluation
				_, err = config.RedisJSONHandler.JSONSet(newComment.UserIdentifier, ".", userBucket)

				if err != nil {
					http.Error(w, UnableToProcess, http.StatusInternalServerError)
					return
				}
			} else {
				// user already exist, pull the rate limiting state from Redis
				// and deserialize it into the bucket
				bucketJSON, err := redis.Bytes(jsonData, err)
				if err != nil {
					http.Error(w, UnableToProcess, http.StatusInternalServerError)
					return
				}

				err = json.Unmarshal(bucketJSON, userBucket)
				if err != nil {
					http.Error(w, UnableToProcess, http.StatusInternalServerError)
					return
				}
			}

			_, err = userBucket.verifyCommentAllowance()
			if err != nil {
				http.Error(w, RateLimitExceeded, http.StatusTooManyRequests)
				return
			}

			// update the users bucket state after evaluation
			_, err = config.RedisJSONHandler.JSONSet(newComment.UserIdentifier, ".", userBucket)
			if err != nil {
				http.Error(w, UnableToProcess, http.StatusInternalServerError)
				return
			}

			newComment = config.CommentStore.addComment(newComment)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(newComment)
		} else {
			http.Error(w, MethodNotAllowed, http.StatusMethodNotAllowed)
		}
	}, nil
}

func main() {
	// var addr = flag.String("Server", "localhost:6379", "Redis server address")
	// flag.Parse()

	rs, rh := getRedSyncInstance()
	handlerConfiguration := CommentHandlerConfig{
		RedisJSONHandler: rh,
		CommentStore:     &Comments{},
		FillRate:         StandardFillRate,
		BucketCapacity:   StandardBucketCapacity,
		FillUnit:         StandardUnit,
	}

	redisMtxHandler := RedisMutexHandler{
		redSync: rs,
	}
	handlerConfiguration.RedisMu = &redisMtxHandler
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
func (b *Bucket) verifyCommentAllowance() (time.Time, error) {
	currentTime := time.Now()

	// if the number of tokens is non-empty, we know the request is
	// ok to process. From here, we can drain a token and update
	// the evaluation time.
	if b.Tokens > 0 {
		b.Tokens--
		b.LastChecked = currentTime
		return currentTime, nil
	}

	// threshold calculates a delta between now and whatever our fill rate is in the past
	// to evaluate if we're able to process the request or not.
	threshold := currentTime.Add(-1 * b.FillRate * b.Unit)

	// If the current time is 11:00AM and the lastChecked time is 10:59AM
	// we know that we can only add a token and process the request if
	// the lastChecked time happened before the threshold
	if b.LastChecked.Before(threshold) {
		b.LastChecked = currentTime
		return currentTime, nil
	}

	return time.Now(), fmt.Errorf("unable to process comment as last added comment was: %v and current time is: %v",
		b.LastChecked,
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

// TODO: Don't hardcode
func getRedSyncInstance() (*redsync.Redsync, *rejson.Handler) {
	client := goredislib.NewClient(&goredislib.Options{
		Addr: "localhost:6379",
	})
	pool := goredis.NewPool(client)
	rh := rejson.NewReJSONHandler()
	rh.SetGoRedisClientWithContext(context.Background(), client)

	return redsync.New(pool), rh
}
