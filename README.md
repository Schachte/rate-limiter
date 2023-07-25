# Distributed Rate Limiter

![Test Suite](https://github.com/schachte/rate-limiter/actions/workflows/run_go_tests.yml/badge.svg)


Simple and scalable rate limiter that leverages the token bucket algorithm, written in Go. 

# Usage

```go
// Redis connection host and port
connection := "localhost:6379"

// Obtain Redlock implementation for distributed locking
redSync, redisJsonHandler := GetRedSyncInstance(&connection)
redisMtxHandler := RedisMutexHandler{
    redSync: redSync,
}

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

// Or extend usage by initializing a sidecar proxy for all your
// applications
err = rateLimiter.StartServer()
if err != nil {
    log.Fatalf("unable to start rate limiting server %v", err)
}
```

# Development

See above example for usage. 

- `NewRateLimiter` can use the algorithm as a standard, importable library function
- `StartServer` will execute an HTTP server for proxy usage to be used as a sidecar or self-hosted service

## Build
```sh
make build
```

## Tests
```sh
# run test suite
make test

# remove test cache
make wipe-cache 
```

## Clean
```sh
make clean
```


# Supported:

- [Redlock](https://redis.com/glossary/redlock/) for distributed locking
- [Redis](https://redis.io/) for optimized caching
- Rate limiting with the [token bucket algorithm](https://en.wikipedia.org/wiki/Token_bucket)
- Easy to use library
- Optional web proxy for sidecar usage

# Roadmap

[Planning/Roadmap](https://github.com/users/Schachte/projects/4)
