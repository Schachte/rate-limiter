package main

import (
	"context"
	"fmt"
	"time"

	"github.com/go-redsync/redsync/v4"
	"github.com/go-redsync/redsync/v4/redis/goredis/v9"
	"github.com/nitishm/go-rejson/v4"
	"github.com/nitishm/go-rejson/v4/rjs"
	goredislib "github.com/redis/go-redis/v9"
)

type RedisStore interface {
	JSONGet(key, path string, opts ...rjs.GetOption) (res interface{}, err error)
	JSONSet(key string, path string, obj interface{}, opts ...rjs.SetOption) (res interface{}, err error)
}

const (
	// StandardBucketCapacity is the volume of the bucket per-user representing how many
	// tokens will fit in the bucket
	StandardBucketCapacity = 1
	// StandardFillRate is the token refill rate in conjunction with the StandardUnit
	StandardFillRate = 30
	// StandardUnit is the unit used to determine cadence of the token refill
	StandardUnit = time.Second
)

// RateLimitConfig contains all the information used to dynamically
// configure our handler with different rates and storage providers
type RateLimitConfig struct {
	RedisMu          RedisMutex
	RedisJSONHandler RedisStore
	BucketCapacity   int
	FillRate         time.Duration
	FillUnit         time.Duration
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

// verifyAllowance will employ the token bucket algorithm which
// will reference a specific users usage quota to determine if the comment
// can be added or not.
func (b *Bucket) verifyAllowance() (time.Time, error) {
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

// GetRedSyncInstance will initialize redsync for distributed locking
// and return the Redis JSON handler used for persisting JSON into Redis
func GetRedSyncInstance(connection *string) (*redsync.Redsync, *rejson.Handler) {
	client := goredislib.NewClient(&goredislib.Options{
		Addr: *connection,
	})
	pool := goredis.NewPool(client)
	rh := rejson.NewReJSONHandler()
	rh.SetGoRedisClientWithContext(context.Background(), client)
	return redsync.New(pool), rh
}
