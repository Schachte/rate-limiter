package ratelimiter

import "github.com/go-redsync/redsync/v4"

type RedisMutex interface {
	NewMutex(name string, options ...redsync.Option) MutexLock
}

type MutexLock interface {
	Lock() error
	Unlock() (bool, error)
}

type RedisMutexHandler struct {
	redSync *redsync.Redsync
}

type RedisMutexLock struct {
	lock redsync.Mutex
}

func (m *RedisMutexHandler) NewMutex(name string, options ...redsync.Option) MutexLock {
	return m.redSync.NewMutex(name)
}

func (m *RedisMutexLock) Lock() error {
	return m.lock.Lock()
}

func (m *RedisMutexLock) Unlock() (bool, error) {
	return m.lock.Unlock()
}
