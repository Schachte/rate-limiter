package main

import (
	"log"
)

type Request struct {
	UserIdentifier string `json:"UserIdentifier"`
}

func main() {
	// TODO: Pull this in via flag
	connection := "localhost:6379"
	redSync, redisJsonHandler := GetRedSyncInstance(&connection)
	redisMtxHandler := RedisMutexHandler{
		redSync: redSync,
	}
	cfg := RateLimitConfig{
		RedisHostname: "localhost",
		RedisPort:     6379,
		ServerHost:    "0.0.0.0",
		ServerPort:    8080,

		FillRate:       StandardFillRate,
		BucketCapacity: StandardBucketCapacity,
		FillUnit:       StandardUnit,

		RedisMu:          &redisMtxHandler,
		RedisJSONHandler: redisJsonHandler,
	}

	rateLimiter, err := NewRateLimiter(cfg)
	if err != nil {
		log.Fatalf("unable to initialize rate limiter %v", err)
	}

	err = rateLimiter.StartServer()
	if err != nil {
		log.Fatalf("unable to start rate limiting server %v", err)
	}
}
