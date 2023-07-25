package ratelimiter

import (
	"net/http"
)

type Error struct {
	Message    string
	StatusCode int
	Err        error
}

func (e Error) Error() string {
	return e.Message
}

func (e Error) WithError(err error) Error {
	e.Err = err
	return e
}

const (
	UnableToProcess          = "unable to process request, please try again"
	RateLimitExceeded        = "rate limit exceeded, try again later"
	MethodNotAllowed         = "method not allowed"
	UnableToAcquireLock      = "unable to acquire lock"
	UnableToReleaseLock      = "unable to release lock"
	UnableToSetJSONKey       = "unable to set JSON data for user"
	UnableToGetJSONKey       = "unable to get JSON data for user"
	UnableToDeserialize      = "unable to deserialize bytes"
	UnableToRetrieveRedisKey = "unable to retrieve key from redis store"
	UnableToPersistMetadata  = "unable to persist metadata"
	MissingIdentifierHeader  = "missing User-Identifier header"
)

func NewUnableToProcessError() Error {
	return Error{
		Message:    UnableToProcess,
		StatusCode: http.StatusInternalServerError,
	}
}

func NewRateLimitExceeded() Error {
	return Error{
		Message:    RateLimitExceeded,
		StatusCode: http.StatusTooManyRequests,
	}
}

func NewUserIdentifierMissing() Error {
	return Error{
		Message:    MissingIdentifierHeader,
		StatusCode: http.StatusBadRequest,
	}
}

func NewUnableToAcquireLock() Error {
	return Error{
		Message:    UnableToAcquireLock,
		StatusCode: http.StatusInternalServerError,
	}
}

func NewUnableToReleaseLock() Error {
	return Error{
		Message:    UnableToReleaseLock,
		StatusCode: http.StatusInternalServerError,
	}
}

func NewUnableToGetJSONKey() Error {
	return Error{
		Message:    UnableToRetrieveRedisKey,
		StatusCode: http.StatusInternalServerError,
	}
}

func NewUnableToSetJSONKey() Error {
	return Error{
		Message:    UnableToRetrieveRedisKey,
		StatusCode: http.StatusInternalServerError,
	}
}

func NewUnableToDeserialize() Error {
	return Error{
		Message:    UnableToDeserialize,
		StatusCode: http.StatusInternalServerError,
	}
}

func NewUnableToPersistMetadata() Error {
	return Error{
		Message:    UnableToPersistMetadata,
		StatusCode: http.StatusInternalServerError,
	}
}
