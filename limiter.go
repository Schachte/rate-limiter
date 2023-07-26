package ratelimiter

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-redsync/redsync/v4"
	"github.com/go-redsync/redsync/v4/redis/goredis/v9"
	"github.com/gomodule/redigo/redis"
	"github.com/nitishm/go-rejson/v4/rjs"
	goredislib "github.com/redis/go-redis/v9"
	"go.uber.org/zap"
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

// Request contains information required to uniquely identify a request. This
// is typically pulled from the User-Identifier HTTP header for the sidecar proxy
type Request struct {
	UserIdentifier string `json:"UserIdentifier"`
}

// RateLimitConfig contains all the information used to dynamically
// configure our handler with different rates and storage providers
type RateLimitConfig struct {
	RedisMu          RedisMutex
	RedisJSONHandler RedisStore
	BucketCapacity   int
	FillRate         time.Duration
	FillUnit         time.Duration
	RedisHostname    string
	RedisPort        int

	ServerHost string
	ServerPort int
}

type RateLimiter struct {
	cfg    RateLimitConfig
	logger *zap.Logger
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
	// Capacity is the total capacity of the bucket
	Capacity int `json:"Capacity"`
}

// EvaluateRequest will lock the user identifier in Redis and evaluate if the configured
// token limits for the user have been exceeded
func (r *RateLimiter) EvaluateRequest(incomingReq Request) (evalError error) {
	if incomingReq.UserIdentifier == "" {
		return NewUserIdentifierMissing()
	}
	mtx := r.cfg.RedisMu.NewMutex(fmt.Sprintf("%s-lock", incomingReq.UserIdentifier))
	if err := mtx.Lock(); err != nil {
		return NewUnableToAcquireLock().WithError(err)
	}
	defer func() {
		// TODO: This return isn't handled properly
		if ok, err := mtx.Unlock(); !ok || err != nil {
			evalError = NewUnableToReleaseLock().WithError(err)
		}
	}()

	userBucket := &Bucket{}
	jsonData, err := r.cfg.RedisJSONHandler.JSONGet(incomingReq.UserIdentifier, ".")
	if err != nil {
		if err != goredislib.Nil {
			return NewUnableToGetJSONKey().WithError(err)
		}

		// in this scenario, we just need to store the new user that we haven't processed before
		userBucket = &Bucket{
			UserIdentifier: incomingReq.UserIdentifier,
			FillRate:       r.cfg.FillRate,
			Tokens:         0,
			Unit:           r.cfg.FillUnit,
			Capacity:       r.cfg.BucketCapacity,
		}

		_, err = r.cfg.RedisJSONHandler.JSONSet(incomingReq.UserIdentifier, ".", userBucket)
		if err != nil {
			return NewUnableToSetJSONKey().WithError(err)
		}
	} else {
		bucketJSON, err := redis.Bytes(jsonData, err)
		if err != nil {
			return NewUnableToDeserialize().WithError(err)
		}

		err = json.Unmarshal(bucketJSON, userBucket)
		if err != nil {
			return NewUnableToDeserialize().WithError(err)
		}
	}

	_, err = userBucket.verifyAllowance()
	if err != nil {
		return errors.New(RateLimitExceeded)
	}

	_, err = r.cfg.RedisJSONHandler.JSONSet(incomingReq.UserIdentifier, ".", userBucket)
	if err != nil {
		return NewUnableToPersistMetadata().WithError(err)
	}
	return nil
}

// verifyAllowance will employ the token bucket algorithm which
// will reference a specific users usage quota to determine if the comment
// can be added or not.
func (b *Bucket) verifyAllowance() (time.Time, error) {
	currentTime := time.Now()
	timeElapsedSeconds := currentTime.Sub(b.LastChecked).Seconds()

	// calculate number of tokens to retroactively add into the bucket
	newTokens := int(timeElapsedSeconds) / int(b.FillRate)

	// avoid exceeding max bucket capacity
	if newTokens > int(b.Capacity) {
		newTokens = int(b.Capacity)
	}

	// update the number of tokens in the bucket
	b.Tokens += int(newTokens)
	if b.Tokens > b.Capacity {
		b.Tokens = b.Capacity
	}

	// update the last checked time
	b.LastChecked = currentTime

	// if the number of tokens is non-empty, we know the request is
	// ok to process. From here, we can drain a token and update
	// the evaluation time.
	if b.Tokens > 0 {
		b.Tokens -= 1
		return currentTime, nil
	}

	return time.Now(), fmt.Errorf("unable to process comment as last added comment was: %v and current time is: %v",
		b.LastChecked,
		currentTime,
	)
}

// GetRedSyncInstance will initialize redsync for distributed locking
// and return the Redis JSON handler used for persisting JSON into Redis
func GetRedSyncInstance(client *goredislib.Client) *redsync.Redsync {
	pool := goredis.NewPool(client)
	return redsync.New(pool)
}

// InitializeWebLimiter provides a web handler to leverage rate limiting via a web proxy
// or sidecar container. This is a good choice when deploying usage for all applications if you
// prefer to avoid embedding rate limiting logic directly into your application code.
func NewRateLimiter(config RateLimitConfig) (RateLimiter, error) {
	logger, err := zap.NewProduction()
	if err != nil {
		panic(err)
	}
	defer logger.Sync()
	return RateLimiter{
		config,
		logger,
	}, nil
}

// RateLimitHandler will evaluate each request on a per-user basis and determine if the request should
// be permitted
func (l *RateLimiter) RateLimitHandler() (func(w http.ResponseWriter, r *http.Request), error) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			userIdentifier := r.Header.Get("User-Identifier")
			if userIdentifier == "" {
				http.Error(w, MissingIdentifierHeader, http.StatusBadRequest)
				return
			}

			var newReq Request
			err := json.NewDecoder(r.Body).Decode(&newReq)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			err = l.EvaluateRequest(newReq)
			if err != nil {
				if err.Error() == RateLimitExceeded {
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, MethodNotAllowed, http.StatusMethodNotAllowed)
	}, nil
}

// StartServer will run a web server that can receive requests
// for rate limit evaluation
func (r *RateLimiter) StartServer() error {
	rateLimiter, err := r.RateLimitHandler()
	if err != nil {
		return errors.New("unable to initialize rate limit handler")
	}

	http.HandleFunc("/limiter", rateLimiter)
	fmt.Printf("Running server at %s:%d\n", r.cfg.ServerHost, r.cfg.ServerPort)
	return http.ListenAndServe(
		fmt.Sprintf(
			"%s:%d",
			r.cfg.ServerHost,
			r.cfg.ServerPort,
		), nil)
}
