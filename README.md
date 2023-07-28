# Distributed Rate Limiter

![Test Suite](https://github.com/schachte/rate-limiter/actions/workflows/run_go_tests.yml/badge.svg)


Simple and scalable rate limiter that leverages the token bucket algorithm. Useful as a sidecar proxy or direct integration into your application. See usage below. 

Rate limiters are useful within an application or API gateway/sidecar proxy to avoid overloading your origin server or for preventing spam and abuse. When too many requests are invoked within a configurable time period, the user will be hit with either `TooManyRequests` error in Go or the equivalent `429 TooManyRequests` HTTP status code if using the web server.

Currently, the only dependency is Redis, which is used as a caching server for the bucket statistics to track per-ID request usage. Additionally, Redsync is used internally which implements the [Redlock](https://redis.com/glossary/redlock/) algorithm for distributed locking to prevent race conditions from happening when calculating token usage.

# Usage

```
Options:
  -bucket-capacity int
        Bucket capacity (default 1)
  -fill-rate float
        Fill rate (default 30)
  -fill-unit duration
        Fill unit (default 1s)
  -help
        Display help
  -redis-hostname string
        Redis hostname (default "redis")
  -redis-port int
        Redis port (default 6379)
  -server-host string
        Server host (default "0.0.0.0")
  -server-port int
        Server port (default 8080)
```

Run local environment locally via Docker with: `make run-dev`

You can view examples in the [cli](/cli) directory.

```go
limiter "github.com/schachte/rate-limiter"

// Define parameters for Redis connection and token bucket
// parameters
cfg := limiter.RateLimitConfig{
    RedisHostname: "localhost",
    RedisPort:     6379,
    ServerHost:    "0.0.0.0",
    ServerPort:    9080,

    FillRate:       limiter.StandardFillRate,
    BucketCapacity: limiter.StandardBucketCapacity,
    FillUnit:       limiter.StandardUnit,
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
```

# Docker

You can run the entire development server via Docker by running `make run-dev`. Alternatively, you can run the `docker-compose.yml` file directly and target
only the rate limiter service if you already have Redis running with `docker-compose up -d limiter`

# Development

See above example for usage. 

- `NewRateLimiter` can use the algorithm as a standard, importable library function
- `StartServer` will execute an HTTP server for proxy usage to be used as a sidecar or self-hosted service

## Make

```sh
make build
```

```sh
# run test suite
make test

# remove test cache
make wipe-cache 
```

```sh
make clean
```

## Docker

You can run Redis via Docker easily

```sh
make run-dev
```

```sh
make destroy-dev
```


# Supported:

- [Redlock](https://redis.com/glossary/redlock/) for distributed locking
- [Redis](https://redis.io/) for optimized caching
- Rate limiting with the [token bucket algorithm](https://en.wikipedia.org/wiki/Token_bucket)
- Easy to use library
- Optional web proxy for sidecar usage

# Roadmap

[Planning/Roadmap](https://github.com/users/Schachte/projects/4)
