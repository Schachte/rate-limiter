package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"

	"github.com/gomodule/redigo/redis"
	goredislib "github.com/redis/go-redis/v9"
)

type Request struct {
	UserIdentifier string `json:"UserIdentifier"`
}

// rateLimitHandler will evaluate each request on a per-user basis and determine if the request should
// be permitted
func rateLimitHandler(config *RateLimitConfig) (func(w http.ResponseWriter, r *http.Request), error) {
	if config == nil {
		return nil, errors.New("missing handler configuration")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var newReq Request
			err := json.NewDecoder(r.Body).Decode(&newReq)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}

			mtx := config.RedisMu.NewMutex(fmt.Sprintf("%s-lock", newReq.UserIdentifier))
			if err := mtx.Lock(); err != nil {
				http.Error(w, UnableToProcess, http.StatusInternalServerError)
				return
			}
			defer func() {
				if ok, err := mtx.Unlock(); !ok || err != nil {
					http.Error(w, UnableToProcess, http.StatusInternalServerError)
					return
				}
			}()
			userBucket := &Bucket{}
			jsonData, err := config.RedisJSONHandler.JSONGet(newReq.UserIdentifier, ".")
			if err != nil {
				if err != goredislib.Nil {
					http.Error(w, UnableToProcess, http.StatusInternalServerError)
					return
				}
				// in this scenario, we just need to store the new user that we haven't processed before
				userBucket = &Bucket{
					UserIdentifier: newReq.UserIdentifier,
					FillRate:       config.FillRate,
					Tokens:         config.BucketCapacity,
					Unit:           config.FillUnit,
				}

				// update the users bucket state after evaluation
				_, err = config.RedisJSONHandler.JSONSet(newReq.UserIdentifier, ".", userBucket)

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

			_, err = userBucket.verifyAllowance()
			if err != nil {
				http.Error(w, RateLimitExceeded, http.StatusTooManyRequests)
				return
			}

			// update the users bucket state after evaluation
			_, err = config.RedisJSONHandler.JSONSet(newReq.UserIdentifier, ".", userBucket)
			if err != nil {
				http.Error(w, UnableToProcess, http.StatusInternalServerError)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
		} else {
			http.Error(w, MethodNotAllowed, http.StatusMethodNotAllowed)
		}
	}, nil
}

func main() {
	var addr = flag.String("Server", "localhost:6379", "Redis server address")
	flag.Parse()

	rs, rh := GetRedSyncInstance(addr)
	redisMtxHandler := RedisMutexHandler{
		redSync: rs,
	}
	handlerConfiguration := RateLimitConfig{
		RedisMu:          &redisMtxHandler,
		RedisJSONHandler: rh,
		FillRate:         StandardFillRate,
		BucketCapacity:   StandardBucketCapacity,
		FillUnit:         StandardUnit,
	}
	rateLimiter, err := rateLimitHandler(&handlerConfiguration)
	if err != nil {
		log.Fatal("unable to initialize handler with missing configuration")
	}

	http.HandleFunc("/limiter", rateLimiter)
	http.ListenAndServe(":8080", nil)
}
