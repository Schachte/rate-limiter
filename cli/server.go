package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	limiter "github.com/schachte/rate-limiter"
)

// Demonstrates examples, but also exposes usage to run via CLI from development
// or within a container like Docker, etc.
func main() {
	err := RunServer()
	if err != nil {
		log.Fatal(err)
	}
}

// RunServerExample demonstrates how to run the rate limiter server, which
// is typically used as a proxy to your actual application. This is best suited behind
// a reverse proxy like NGINX that would handle TLS termination upstream
func RunServer() error {
	redisHostname := flag.String("redis-hostname", "redis", "Redis hostname")
	redisPort := flag.Int("redis-port", 6379, "Redis port")
	serverHost := flag.String("server-host", "0.0.0.0", "Server host")
	serverPort := flag.Int("server-port", 8080, "Server port")
	fillRate := flag.Float64("fill-rate", limiter.StandardFillRate, "Fill rate")
	bucketCapacity := flag.Int("bucket-capacity", limiter.StandardBucketCapacity, "Bucket capacity")
	fillUnit := flag.Duration("fill-unit", limiter.StandardUnit, "Fill unit")

	help := flag.Bool("help", false, "Display help")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options]\n\nOptions:\n", os.Args[0])
		flag.PrintDefaults()
	}

	flag.Parse()

	if *help {
		flag.Usage()
		os.Exit(0)
	}

	cfg := limiter.RateLimitConfig{
		RedisHostname: *redisHostname,
		RedisPort:     *redisPort,
		ServerHost:    *serverHost,
		ServerPort:    *serverPort,

		FillRate:       time.Duration(*fillRate),
		BucketCapacity: *bucketCapacity,
		FillUnit:       *fillUnit,
	}

	// Use as a standalone library function
	rateLimiter, err := limiter.NewRateLimiter(cfg)
	if err != nil {
		log.Fatalf("unable to initialize rate limiter %v", err)
	}

	// Or extend usage by initializing a sidecar proxy for all your
	// applications: (http://localhost:8080/limiter)
	err = rateLimiter.StartServer()
	if err != nil {
		return errors.New("unable to start rate limiting server")
	}
	return err
}

// RunWithoutServerExample demonstrates usage without an additional server or container
// if you want to integrate directly into your application. This is good for use as a basic
// rate limiter while still benefitting from the scale Redis offers, but keeping the logic
// self-contained in your application code instead of an external service.
func RunWithoutServerExample() error {
	// Define parameters for Redis connection and token bucket
	// parameters
	cfg := limiter.RateLimitConfig{
		RedisHostname: "redis",
		RedisPort:     6379,
		ServerHost:    "0.0.0.0",
		ServerPort:    8080,

		FillRate:       limiter.StandardFillRate,
		BucketCapacity: limiter.StandardBucketCapacity,
		FillUnit:       limiter.StandardUnit,
	}

	// Use as a standalone library function
	rateLimiter, err := limiter.NewRateLimiter(cfg)
	if err != nil {
		log.Fatalf("unable to initialize rate limiter %v", err)
	}

	req := limiter.Request{
		UserIdentifier: "some-user",
	}
	err = rateLimiter.EvaluateRequest(req)
	if err.(limiter.Error).Error() == limiter.RateLimitExceeded {
		// handle rate limiting errors
		return err
	}
	return err
}
