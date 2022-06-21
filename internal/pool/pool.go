package pool

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-redis/redis/v9/internal"
)

var (
	// ErrClosed performs any operation on the closed client will return this error.
	ErrClosed = errors.New("redis: client is closed")

	// ErrPoolTimeout timed out waiting to get a connection from the connection pool.
	ErrPoolTimeout = errors.New("redis: connection pool timeout")
)

var timers = sync.Pool{
	New: func() interface{} {
		t := time.NewTimer(time.Hour)
		t.Stop()
		return t
	},
}

// Stats contains pool state information and accumulated stats.
type Stats struct {
	Hits     uint32 // number of times free connection was found in the pool
	Misses   uint32 // number of times free connection was NOT found in the pool
	Timeouts uint32 // number of times a wait timeout occurred

	TotalConns uint32 // number of total connections in the pool
	IdleConns  uint32 // number of idle connections in the pool
	StaleConns uint32 // number of stale connections removed from the pool
}

type Pooler interface {
	NewConn(context.Context) (*Conn, error)
	CloseConn(*Conn) error

	Get(context.Context) (*Conn, error)
	Put(context.Context, *Conn)
	Remove(context.Context, *Conn, error)

	Len() int
	IdleLen() int
	Stats() *Stats

	Close() error
}

type Options struct {
	Dialer  func(context.Context) (net.Conn, error)
	OnClose func(*Conn) error

	PoolFIFO           bool
	PoolSize           int
	MinIdleConns       int
	MaxConnAge         time.Duration
	PoolTimeout        time.Duration
	IdleTimeout        time.Duration
	IdleCheckFrequency time.Duration
}

type lastDialErrorWrap struct {
	err error
}

type ConnPool struct {
	opt *Options

	dialErrorsNum uint32 // atomic

	lastDialError atomic.Value

	queue chan struct{}

	connsMu      sync.Mutex
	conns        []*Conn
	idleConns    []*Conn
	poolSize     int
	idleConnsLen int

	stats Stats

	_closed  uint32 // atomic
	closedCh chan struct{}
}

var _ Pooler = (*ConnPool)(nil)

func NewConnPool(opt *Options) *ConnPool {
	// 连接池的 具体 实现
	p := &ConnPool{
		opt: opt,

		queue:     make(chan struct{}, opt.PoolSize), // 开关，链接中可以使用的 todo:
		conns:     make([]*Conn, 0, opt.PoolSize),    // 链接集合
		idleConns: make([]*Conn, 0, opt.PoolSize),    // 空闲链接集合
		closedCh:  make(chan struct{}),
	}

	p.connsMu.Lock()
	p.checkMinIdleConns() // 按照 MinIdle 选项，提前生成指定数量的链接
	p.connsMu.Unlock()

	// 如果设置了闲置链接的过期时间、限制链接的清理频率，则需要对闲置链接进行清理
	if opt.IdleTimeout > 0 && opt.IdleCheckFrequency > 0 {
		// 定期清理
		go p.reaper(opt.IdleCheckFrequency)
	}

	return p
}

// checkMinIdleConns 检查空闲链接数量，补充新连接
func (p *ConnPool) checkMinIdleConns() {
	if p.opt.MinIdleConns == 0 {
		return
	}

	// 当池中链接数量少于
	for p.poolSize < p.opt.PoolSize && p.idleConnsLen < p.opt.MinIdleConns {
		p.poolSize++
		p.idleConnsLen++

		go func() {
			err := p.addIdleConn()
			if err != nil && err != ErrClosed {
				p.connsMu.Lock()
				p.poolSize--
				p.idleConnsLen--
				p.connsMu.Unlock()
			}
		}()
	}
}

// addIdleConn 添加空闲链接
func (p *ConnPool) addIdleConn() error {
	// 创建链接
	cn, err := p.dialConn(context.TODO(), true)
	if err != nil {
		return err
	}

	p.connsMu.Lock()
	defer p.connsMu.Unlock()

	// It is not allowed to add new connections to the closed connection pool.
	if p.closed() {
		_ = cn.Close()
		return ErrClosed
	}

	// 将创建好的链接添加至队列里
	p.conns = append(p.conns, cn)
	p.idleConns = append(p.idleConns, cn)
	return nil
}

func (p *ConnPool) NewConn(ctx context.Context) (*Conn, error) {
	return p.newConn(ctx, false)
}

func (p *ConnPool) newConn(ctx context.Context, pooled bool) (*Conn, error) {
	cn, err := p.dialConn(ctx, pooled)
	if err != nil {
		return nil, err
	}

	p.connsMu.Lock()
	defer p.connsMu.Unlock()

	// It is not allowed to add new connections to the closed connection pool.
	if p.closed() {
		_ = cn.Close()
		return nil, ErrClosed
	}

	p.conns = append(p.conns, cn)
	if pooled {
		// If pool is full remove the cn on next Put.
		if p.poolSize >= p.opt.PoolSize {
			cn.pooled = false
		} else {
			p.poolSize++
		}
	}

	return cn, nil
}

func (p *ConnPool) dialConn(ctx context.Context, pooled bool) (*Conn, error) {
	// 判断 连接池 是否被关闭
	if p.closed() {
		return nil, ErrClosed
	}

	if atomic.LoadUint32(&p.dialErrorsNum) >= uint32(p.opt.PoolSize) {
		return nil, p.getLastDialError()
	}

	// 调用选项中设置的 Dial 函数，创建 net.Conn 链接
	netConn, err := p.opt.Dialer(ctx)
	if err != nil {
		p.setLastDialError(err)
		// 重试
		if atomic.AddUint32(&p.dialErrorsNum, 1) == uint32(p.opt.PoolSize) {
			go p.tryDial()
		}
		return nil, err
	}

	// 封装 net.Conn 链接
	// 添加 Redis 协议的解析生成，并对 Write 增加 bufio
	cn := NewConn(netConn)
	cn.pooled = pooled
	return cn, nil
}

func (p *ConnPool) tryDial() {
	for {
		if p.closed() {
			return
		}

		conn, err := p.opt.Dialer(context.Background())
		if err != nil {
			p.setLastDialError(err)
			time.Sleep(time.Second)
			continue
		}

		atomic.StoreUint32(&p.dialErrorsNum, 0)
		_ = conn.Close()
		return
	}
}

func (p *ConnPool) setLastDialError(err error) {
	p.lastDialError.Store(&lastDialErrorWrap{err: err})
}

func (p *ConnPool) getLastDialError() error {
	err, _ := p.lastDialError.Load().(*lastDialErrorWrap)
	if err != nil {
		return err.err
	}
	return nil
}

// Get returns existed connection from the pool or creates a new one.
func (p *ConnPool) Get(ctx context.Context) (*Conn, error) {
	if p.closed() {
		return nil, ErrClosed
	}

	if err := p.waitTurn(ctx); err != nil {
		return nil, err
	}

	for {
		// 尝试从 idle 队列里找到一个链接
		p.connsMu.Lock()
		cn, err := p.popIdle()
		p.connsMu.Unlock()

		if err != nil {
			return nil, err
		}

		if cn == nil {
			break
		}

		// 判断链接是否超时
		// 超时则重新获取一个新的
		if p.isStaleConn(cn) {
			_ = p.CloseConn(cn)
			continue
		}

		atomic.AddUint32(&p.stats.Hits, 1)
		return cn, nil
	}

	atomic.AddUint32(&p.stats.Misses, 1)

	// 如果遍历了所有 idle 队列，也没有找到一个可用的链接
	// 则会重新创建一个
	newcn, err := p.newConn(ctx, true)
	if err != nil {
		p.freeTurn()
		return nil, err
	}

	return newcn, nil
}

func (p *ConnPool) getTurn() {
	p.queue <- struct{}{}
}

// waitTurn 判断链接是否可用 todo:
func (p *ConnPool) waitTurn(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	select {
	case p.queue <- struct{}{}:
		return nil
	default:
	}

	// 从定时器池中获得一个定时器
	timer := timers.Get().(*time.Timer)
	timer.Reset(p.opt.PoolTimeout)

	// 等待，池中有可以使用的链接
	select {
	case <-ctx.Done():
		if !timer.Stop() {
			<-timer.C
		}
		timers.Put(timer)
		return ctx.Err()
	case p.queue <- struct{}{}:
		if !timer.Stop() {
			<-timer.C
		}
		timers.Put(timer)
		return nil
	case <-timer.C:
		timers.Put(timer)
		atomic.AddUint32(&p.stats.Timeouts, 1)
		return ErrPoolTimeout
	}
}

func (p *ConnPool) freeTurn() {
	<-p.queue
}

func (p *ConnPool) popIdle() (*Conn, error) {
	if p.closed() {
		return nil, ErrClosed
	}
	n := len(p.idleConns)
	if n == 0 {
		return nil, nil
	}

	// 根据是否是 FIFO 从 队列里取出链接
	// 由于 FIFO 涉及到 copy 操作，所以效率会低
	var cn *Conn
	if p.opt.PoolFIFO {
		cn = p.idleConns[0]
		copy(p.idleConns, p.idleConns[1:])
		p.idleConns = p.idleConns[:n-1]
	} else {
		idx := n - 1
		cn = p.idleConns[idx]
		p.idleConns = p.idleConns[:idx]
	}
	p.idleConnsLen--
	p.checkMinIdleConns() // 补充新连接
	return cn, nil
}

func (p *ConnPool) Put(ctx context.Context, cn *Conn) {
	if cn.rd.Buffered() > 0 {
		internal.Logger.Printf(ctx, "Conn has unread data")
		p.Remove(ctx, cn, BadConnError{})
		return
	}

	if !cn.pooled {
		p.Remove(ctx, cn, nil)
		return
	}

	p.connsMu.Lock()
	p.idleConns = append(p.idleConns, cn)
	p.idleConnsLen++
	p.connsMu.Unlock()
	p.freeTurn()
}

func (p *ConnPool) Remove(ctx context.Context, cn *Conn, reason error) {
	p.removeConnWithLock(cn)
	p.freeTurn()
	_ = p.closeConn(cn)
}

func (p *ConnPool) CloseConn(cn *Conn) error {
	p.removeConnWithLock(cn)
	return p.closeConn(cn)
}

func (p *ConnPool) removeConnWithLock(cn *Conn) {
	p.connsMu.Lock()
	p.removeConn(cn)
	p.connsMu.Unlock()
}

func (p *ConnPool) removeConn(cn *Conn) {
	// 遍历所有链接，找到相同的
	for i, c := range p.conns {
		if c == cn {
			p.conns = append(p.conns[:i], p.conns[i+1:]...)
			if cn.pooled {
				p.poolSize--

				// 如果数量不够，则需要重新新的链接
				p.checkMinIdleConns()
			}
			return
		}
	}
}

func (p *ConnPool) closeConn(cn *Conn) error {
	if p.opt.OnClose != nil {
		_ = p.opt.OnClose(cn)
	}
	return cn.Close()
}

// Len returns total number of connections.
func (p *ConnPool) Len() int {
	p.connsMu.Lock()
	n := len(p.conns)
	p.connsMu.Unlock()
	return n
}

// IdleLen returns number of idle connections.
func (p *ConnPool) IdleLen() int {
	p.connsMu.Lock()
	n := p.idleConnsLen
	p.connsMu.Unlock()
	return n
}

func (p *ConnPool) Stats() *Stats {
	idleLen := p.IdleLen()
	return &Stats{
		Hits:     atomic.LoadUint32(&p.stats.Hits),
		Misses:   atomic.LoadUint32(&p.stats.Misses),
		Timeouts: atomic.LoadUint32(&p.stats.Timeouts),

		TotalConns: uint32(p.Len()),
		IdleConns:  uint32(idleLen),
		StaleConns: atomic.LoadUint32(&p.stats.StaleConns),
	}
}

func (p *ConnPool) closed() bool {
	return atomic.LoadUint32(&p._closed) == 1
}

func (p *ConnPool) Filter(fn func(*Conn) bool) error {
	p.connsMu.Lock()
	defer p.connsMu.Unlock()

	var firstErr error
	for _, cn := range p.conns {
		if fn(cn) {
			if err := p.closeConn(cn); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (p *ConnPool) Close() error {
	if !atomic.CompareAndSwapUint32(&p._closed, 0, 1) {
		return ErrClosed
	}
	close(p.closedCh)

	var firstErr error
	p.connsMu.Lock()
	for _, cn := range p.conns {
		if err := p.closeConn(cn); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	p.conns = nil
	p.poolSize = 0
	p.idleConns = nil
	p.idleConnsLen = 0
	p.connsMu.Unlock()

	return firstErr
}

func (p *ConnPool) reaper(frequency time.Duration) {
	ticker := time.NewTicker(frequency)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// It is possible that ticker and closedCh arrive together,
			// and select pseudo-randomly pick ticker case, we double
			// check here to prevent being executed after closed.

			// 双检查，避免 定时器、关闭通知 同时到达
			// select 会对同时到达的，随机选择
			if p.closed() {
				return
			}

			// 判断、清理旧链接
			// Reap: 收割
			// Stable: 陈旧
			_, err := p.ReapStaleConns()
			if err != nil {
				internal.Logger.Printf(context.Background(), "ReapStaleConns failed: %s", err)
				continue
			}
		case <-p.closedCh:
			return
		}
	}
}

func (p *ConnPool) ReapStaleConns() (int, error) {
	var n int

	// 从头开始逐个判断 idle 队列里的链接
	// 因为 idle，新创建的是插到末尾
	// 所以检测当检测到某个链接没有过期，则后面所有的链接都不会过期
	for {
		p.getTurn()

		p.connsMu.Lock()
		cn := p.reapStaleConn()
		p.connsMu.Unlock()

		p.freeTurn()

		if cn != nil {
			_ = p.closeConn(cn)
			n++
		} else {
			break
		}
	}
	atomic.AddUint32(&p.stats.StaleConns, uint32(n))
	return n, nil
}

func (p *ConnPool) reapStaleConn() *Conn {
	if len(p.idleConns) == 0 {
		return nil
	}

	cn := p.idleConns[0]

	// 判断链接是否过期
	if !p.isStaleConn(cn) {
		return nil
	}

	// 过期，则从队列里移除
	p.idleConns = append(p.idleConns[:0], p.idleConns[1:]...)
	p.idleConnsLen--
	p.removeConn(cn) // 还需要从总池子里删掉该链接

	return cn
}

func (p *ConnPool) isStaleConn(cn *Conn) bool {
	if p.opt.IdleTimeout == 0 && p.opt.MaxConnAge == 0 {
		return connCheck(cn.netConn) != nil
	}

	now := time.Now()
	if p.opt.IdleTimeout > 0 && now.Sub(cn.UsedAt()) >= p.opt.IdleTimeout {
		return true
	}
	if p.opt.MaxConnAge > 0 && now.Sub(cn.createdAt) >= p.opt.MaxConnAge {
		return true
	}

	return connCheck(cn.netConn) != nil
}
