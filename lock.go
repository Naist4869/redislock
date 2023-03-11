package redislock

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/go-redsync/redsync/v4"
	"github.com/go-redsync/redsync/v4/redis/goredis/v9"
	"github.com/redis/go-redis/v9"
)

var ErrLockNameEmpty = errors.New("the lock must be named")

type noCopy struct{}

func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}

type stopAutoExtendLock func()

type (
	Locker interface {
		Get(key string, expire time.Duration, tries int) (Lock, error)
		GetAndLock(ctx context.Context, key string, expire time.Duration, tries int) (lock Lock, err error)
	}

	locker struct {
		redSync *redsync.Redsync
	}

	Lock interface {
		Acquire(context.Context) error
		Release(context.Context) (bool, error)
		Valid() bool
	}

	lock struct {
		noCopy
		expiredAfter   time.Duration
		stopAutoExtend stopAutoExtendLock
		rm             *redsync.Mutex
		stop           chan struct{}
	}
)

func NewLocker(redisClient *redis.Client) Locker {
	return &locker{
		redSync: redsync.New(goredis.NewPool(redisClient)),
	}
}

func (l *locker) Get(key string, expire time.Duration, tries int) (Lock, error) {
	if len(strings.TrimSpace(key)) == 0 {
		return nil, ErrLockNameEmpty
	}

	defaultOption := []redsync.Option{
		redsync.WithExpiry(expire),
		redsync.WithTries(tries),
	}
	mutex := l.redSync.NewMutex(key, defaultOption...)

	return newLock(mutex, expire), nil
}

func (l *locker) GetAndLock(ctx context.Context, key string, expire time.Duration, tries int) (lock Lock, err error) {
	lock, err = l.Get(key, expire, tries)
	if err != nil {
		return nil, err
	}

	return lock, lock.Acquire(ctx)
}

func newLock(m *redsync.Mutex, expiry time.Duration) *lock {
	l := &lock{
		expiredAfter: expiry,
		rm:           m,
		stop:         make(chan struct{}, 1),
	}
	l.stop <- struct{}{}
	return l
}

func (l *lock) autoExtendLock(ctx context.Context, m *redsync.Mutex, dur time.Duration) stopAutoExtendLock {
	extendTicker := time.NewTicker(dur)
	go func() {
		for {
			select {
			case l.stop <- struct{}{}:
				close(l.stop)
				return
			case <-ctx.Done():
				extendTicker.Stop()
				return
			case <-extendTicker.C:
				ok, err := m.ExtendContext(ctx)
				if err != nil || !ok {
					// todo log
					return
				}
			}
		}
	}()
	return func() {
		extendTicker.Stop()
		<-l.stop
	}
}

func (l *lock) Acquire(ctx context.Context) error {
	err := l.rm.LockContext(ctx)
	if err != nil {
		return err
	}
	d := 3 * l.expiredAfter / 4 // ~ 3/4 of the expiry time
	l.stopAutoExtend = l.autoExtendLock(ctx, l.rm, d)
	return nil
}

func (l *lock) Release(ctx context.Context) (bool, error) {
	l.stopAutoExtend()
	res, err := l.rm.UnlockContext(ctx)
	if err != nil {
		return false, err
	}
	return res, nil
}

func (l *lock) Valid() bool {
	return time.Now().Before(l.rm.Until())
}
