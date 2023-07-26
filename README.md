# Distributed Rate Limiter

![Test Suite](https://github.com/schachte/rate-limiter/actions/workflows/run_go_tests.yml/badge.svg)


Simple and scalable rate limiter that leverages the token bucket algorithm. Useful as a sidecar proxy or direct integration into your application. See usage below. 

# Usage

```go
// Define parameters for Redis connection and token bucket
// parameters
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

// Use as a standalone library function
rateLimiter, err := NewRateLimiter(cfg)
if err != nil {
    log.Fatalf("unable to initialize rate limiter %v", err)
}

err := rateLimiter.EvaluateRequest(Request{
    UserIdentifier: "uniqueUserID",
})

if err.(Error).Message == RateLimitExceeded {
    // handle rate limiting logic
}

// Or extend usage by initializing a sidecar proxy for all your
// applications: (http://localhost:8080/limiter)
err = rateLimiter.StartServer()
if err != nil {
    log.Fatalf("unable to start rate limiting server %v", err)
}
```

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
