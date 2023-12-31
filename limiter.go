package ratelimiter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/go-redsync/redsync/v4"
	"github.com/go-redsync/redsync/v4/redis/goredis/v9"
	"github.com/gomodule/redigo/redis"
	"github.com/nitishm/go-rejson/v4"
	"github.com/nitishm/go-rejson/v4/rjs"
	goredislib "github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
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
	UserIdentifier string `json:"User-Identifier"`
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
	UserIdentifier string `json:"User-Identifier"`
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
		r.logger.Debug("request made with no user identifier")
		return NewUserIdentifierMissing()
	}
	mtx := r.cfg.RedisMu.NewMutex(fmt.Sprintf("%s-lock", incomingReq.UserIdentifier))
	if err := mtx.Lock(); err != nil {
		r.logger.Error("unable to acquire mutex on user key",
			zap.String("user", incomingReq.UserIdentifier),
			zap.Error(err),
		)
		return NewUnableToAcquireLock().WithError(err)
	}
	defer func() {
		// TODO: This return isn't handled properly
		if ok, err := mtx.Unlock(); !ok || err != nil {
			r.logger.Error("unable to release mutex on user key",
				zap.String("user", incomingReq.UserIdentifier),
				zap.Error(err),
			)
			evalError = NewUnableToReleaseLock().WithError(err)
		}
	}()

	userBucket := &Bucket{}
	jsonData, err := r.cfg.RedisJSONHandler.JSONGet(incomingReq.UserIdentifier, ".")
	if err != nil {
		if err != goredislib.Nil {
			r.logger.Error("unable to handle key retrieval for user",
				zap.String("user", incomingReq.UserIdentifier),
				zap.Error(err),
			)
			return NewUnableToGetJSONKey().WithError(err)
		}

		// in this scenario, we just need to store the new user that we haven't processed before
		userBucket = &Bucket{
			UserIdentifier: incomingReq.UserIdentifier,
			FillRate:       r.cfg.FillRate,
			Unit:           r.cfg.FillUnit,
			Capacity:       r.cfg.BucketCapacity,
		}

		_, err = r.cfg.RedisJSONHandler.JSONSet(incomingReq.UserIdentifier, ".", userBucket)
		if err != nil {
			r.logger.Error("unable to set JSON entry for user bucket",
				zap.String("user", incomingReq.UserIdentifier),
				zap.Error(err),
			)
			return NewUnableToSetJSONKey().WithError(err)
		}
	} else {
		bucketJSON, err := redis.Bytes(jsonData, err)
		if err != nil {
			r.logger.Error("unable to deserialize JSON entry for user bucket",
				zap.String("user", incomingReq.UserIdentifier),
				zap.Error(err),
			)
			return NewUnableToDeserialize().WithError(err)
		}

		err = json.Unmarshal(bucketJSON, userBucket)
		if err != nil {
			r.logger.Error("unable to deserialize JSON entry for user bucket",
				zap.String("user", incomingReq.UserIdentifier),
				zap.Error(err),
			)
			return NewUnableToDeserialize().WithError(err)
		}
	}

	err = userBucket.verifyAllowance()
	if err != nil {
		r.logger.Error("user has exceeded rate limit, request is denied",
			zap.String("user", incomingReq.UserIdentifier),
			zap.Error(err),
		)
		return errors.New(RateLimitExceeded)
	}

	_, err = r.cfg.RedisJSONHandler.JSONSet(incomingReq.UserIdentifier, ".", userBucket)
	if err != nil {
		r.logger.Error("unable to set JSON entry for user bucket",
			zap.String("user", incomingReq.UserIdentifier),
			zap.Error(err),
		)
		return NewUnableToPersistMetadata().WithError(err)
	}
	return nil
}

// verifyAllowance will employ the token bucket algorithm which
// will reference a specific users usage quota to determine if the comment
// can be added or not.
func (b *Bucket) verifyAllowance() error {
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

	// update the last checked time
	b.LastChecked = currentTime

	// if the number of tokens is non-empty, we know the request is
	// ok to process. From here, we can drain a token and update
	// the evaluation time.
	if b.Tokens > 0 {
		b.Tokens -= 1
		return nil
	}

	return fmt.Errorf("unable to process comment as last added comment was: %v and current time is: %v",
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
	// logger, err := zap.NewProduction()
	// if err != nil {
	// 	panic(err)
	// }
	// defer logger.Sync()

	encoderConfig := zapcore.EncoderConfig{
		TimeKey:        "time",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		MessageKey:     "msg",
		StacktraceKey:  "error",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeDuration: zapcore.StringDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}

	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderConfig),
		zapcore.AddSync(os.Stdout),
		zap.InfoLevel,
	)

	logger := zap.New(core)
	defer logger.Sync()

	switch {
	case config.RedisHostname == "":
		return RateLimiter{}, errors.New("missing redis hostname")
	case config.ServerHost == "":
		return RateLimiter{}, errors.New("missing server hostname")
	case config.RedisPort <= 0:
		return RateLimiter{}, errors.New("invalid redis port")
	case config.ServerPort <= 0:
		return RateLimiter{}, errors.New("invalid redis port")
	case config.FillRate <= 0:
		return RateLimiter{}, errors.New("invalid fill rate")
	}

	client := goredislib.NewClient(&goredislib.Options{
		Addr: fmt.Sprintf("%s:%d", config.RedisHostname, config.RedisPort),
	})

	// Obtain Redlock implementation for distributed locking
	redSync := GetRedSyncInstance(client)
	redisMtxHandler := RedisMutexHandler{
		redSync: redSync,
	}

	// Setup the JSON handler for persisting bucket statistics
	rh := rejson.NewReJSONHandler()
	rh.SetGoRedisClientWithContext(context.Background(), client)
	config.RedisMu = &redisMtxHandler
	config.RedisJSONHandler = rh

	logger.Info(fmt.Sprintf("The rate limiter has been initialized successfully on %s:%d", config.ServerHost, config.ServerPort),
		zap.Int("refill_rate", int(config.FillRate)),
		zap.Int("bucket_capacity", int(config.BucketCapacity)),
		zap.Any("time_unit", config.FillUnit.String()),
	)

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
				l.logger.Debug("request made with no user identifier")
				http.Error(w, MissingIdentifierHeader, http.StatusBadRequest)
				return
			}

			newReq := Request{
				UserIdentifier: userIdentifier,
			}

			w.Header().Set("Content-Type", "application/json")
			err := l.EvaluateRequest(newReq)
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
		r.logger.Error("rate limit handler initialization failed")
		return errors.New("unable to initialize rate limit handler")
	}

	http.HandleFunc("/limiter", rateLimiter)
	r.logger.Info("Running server",
		zap.String("host", r.cfg.ServerHost),
		zap.Int("port", r.cfg.ServerPort),
	)
	return http.ListenAndServe(
		fmt.Sprintf(
			"%s:%d",
			r.cfg.ServerHost,
			r.cfg.ServerPort,
		), nil)
}
