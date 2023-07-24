# Distributed Rate Limiter

This is a work in-progress, generic rate limiting system designed to be used as a sidecar proxy or library.

## Supported:

- [Redlock](https://redis.com/glossary/redlock/) for distributed locking
- [Redis](https://redis.io/) for optimized caching
- Rate limiting with the [token bucket algorithm](https://en.wikipedia.org/wiki/Token_bucket)

## Roadmap

- More generic configuration for sidecar proxy support
- Drop-in replacements for distributed locking with `etcd`
- Prometheus metric export
- Blog article