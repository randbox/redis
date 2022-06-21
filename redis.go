package redis

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/go-redis/redis/v9/internal"
	"github.com/go-redis/redis/v9/internal/pool"
	"github.com/go-redis/redis/v9/internal/proto"
)

// Nil reply returned by Redis when key does not exist.
const Nil = proto.Nil

// SetLogger set custom log
func SetLogger(logger internal.Logging) {
	internal.Logger = logger
}

// ------------------------------------------------------------------------------

type Hook interface {
	BeforeProcess(ctx context.Context, cmd Cmder) (context.Context, error)
	AfterProcess(ctx context.Context, cmd Cmder) error

	BeforeProcessPipeline(ctx context.Context, cmds []Cmder) (context.Context, error)
	AfterProcessPipeline(ctx context.Context, cmds []Cmder) error
}

type hooks struct {
	hooks []Hook
}

func (hs *hooks) lock() {
	hs.hooks = hs.hooks[:len(hs.hooks):len(hs.hooks)]
}

func (hs hooks) clone() hooks {
	clone := hs
	clone.lock()
	return clone
}

func (hs *hooks) AddHook(hook Hook) {
	hs.hooks = append(hs.hooks, hook)
}

func (hs hooks) process(
	ctx context.Context, cmd Cmder, fn func(context.Context, Cmder) error,
) error {
	// 判断是否有 hook，没有则直接调用 fn，也就是 c.Process 传入的 c.baseClient.process 方法
	if len(hs.hooks) == 0 {
		err := fn(ctx, cmd)
		cmd.SetErr(err) // 把 err 添加只 cmd 里
		return err
	}

	var hookIndex int
	var retErr error

	// 如果存在 hook，则会依次执行 hook 的 BeforeProcess 方法
	for ; hookIndex < len(hs.hooks) && retErr == nil; hookIndex++ {
		ctx, retErr = hs.hooks[hookIndex].BeforeProcess(ctx, cmd)
		if retErr != nil {
			cmd.SetErr(retErr)
		}
	}

	// 执行具体的方法
	if retErr == nil {
		retErr = fn(ctx, cmd)
		cmd.SetErr(retErr)
	}

	// 执行完需要再执行 hook 的 AfterProcess
	for hookIndex--; hookIndex >= 0; hookIndex-- {
		if err := hs.hooks[hookIndex].AfterProcess(ctx, cmd); err != nil {
			retErr = err
			cmd.SetErr(retErr)
		}
	}

	return retErr
}

func (hs hooks) processPipeline(
	ctx context.Context, cmds []Cmder, fn func(context.Context, []Cmder) error,
) error {
	if len(hs.hooks) == 0 {
		err := fn(ctx, cmds)
		return err
	}

	var hookIndex int
	var retErr error

	for ; hookIndex < len(hs.hooks) && retErr == nil; hookIndex++ {
		ctx, retErr = hs.hooks[hookIndex].BeforeProcessPipeline(ctx, cmds)
		if retErr != nil {
			setCmdsErr(cmds, retErr)
		}
	}

	if retErr == nil {
		retErr = fn(ctx, cmds)
	}

	for hookIndex--; hookIndex >= 0; hookIndex-- {
		if err := hs.hooks[hookIndex].AfterProcessPipeline(ctx, cmds); err != nil {
			retErr = err
			setCmdsErr(cmds, retErr)
		}
	}

	return retErr
}

func (hs hooks) processTxPipeline(
	ctx context.Context, cmds []Cmder, fn func(context.Context, []Cmder) error,
) error {
	cmds = wrapMultiExec(ctx, cmds)
	return hs.processPipeline(ctx, cmds, fn)
}

// ------------------------------------------------------------------------------

type baseClient struct {
	opt      *Options
	connPool pool.Pooler

	onClose func() error // hook called when client is closed
}

func newBaseClient(opt *Options, connPool pool.Pooler) *baseClient {
	return &baseClient{
		opt:      opt,      // 保存选项
		connPool: connPool, // 连接池
	}
}

func (c *baseClient) clone() *baseClient {
	clone := *c
	return &clone
}

func (c *baseClient) withTimeout(timeout time.Duration) *baseClient {
	opt := c.opt.clone()
	opt.ReadTimeout = timeout
	opt.WriteTimeout = timeout

	clone := c.clone()
	clone.opt = opt

	return clone
}

func (c *baseClient) String() string {
	return fmt.Sprintf("Redis<%s db:%d>", c.getAddr(), c.opt.DB)
}

func (c *baseClient) newConn(ctx context.Context) (*pool.Conn, error) {
	cn, err := c.connPool.NewConn(ctx)
	if err != nil {
		return nil, err
	}

	err = c.initConn(ctx, cn)
	if err != nil {
		_ = c.connPool.CloseConn(cn)
		return nil, err
	}

	return cn, nil
}

func (c *baseClient) getConn(ctx context.Context) (*pool.Conn, error) {
	if c.opt.Limiter != nil {
		// 如果选项中配置了 限流器，则会判断是否被限制
		err := c.opt.Limiter.Allow()
		if err != nil {
			return nil, err
		}
	}

	// 获得链接，并且是 Redis 握手成功，可以直接使用的
	cn, err := c._getConn(ctx)
	if err != nil {
		if c.opt.Limiter != nil {
			// 如果链接报错 todo:
			c.opt.Limiter.ReportResult(err)
		}
		return nil, err
	}

	return cn, nil
}

func (c *baseClient) _getConn(ctx context.Context) (*pool.Conn, error) {
	// 从 连接池 获得一个链接
	// 实际调用的是 *pool.ConnPool 结构的 Get 方法
	// func (p *ConnPool) Get(ctx context.Context) (*Conn, error)
	// 获得的链接是被 Redis 协议封装的
	cn, err := c.connPool.Get(ctx)
	if err != nil {
		return nil, err
	}

	if cn.Inited {
		return cn, nil
	}

	// 如果这个链接的新的，需要做初始化
	// 初始化: 与 Redis 之间的初次握手
	// 完成过后，就是完全可用的链接
	if err := c.initConn(ctx, cn); err != nil {
		c.connPool.Remove(ctx, cn, err)
		if err := errors.Unwrap(err); err != nil {
			return nil, err
		}
		return nil, err
	}

	return cn, nil
}

func (c *baseClient) initConn(ctx context.Context, cn *pool.Conn) error {
	if cn.Inited {
		return nil
	}
	cn.Inited = true

	username, password := c.opt.Username, c.opt.Password
	if c.opt.CredentialsProvider != nil {
		username, password = c.opt.CredentialsProvider()
	}

	connPool := pool.NewSingleConnPool(c.connPool, cn)
	conn := newConn(ctx, c.opt, connPool)

	var auth bool

	// For redis-server <6.0 that does not support the Hello command,
	// we continue to provide services with RESP2.
	if err := conn.Hello(ctx, 3, username, password, "").Err(); err == nil {
		auth = true
	} else if err.Error() != "ERR unknown command 'hello'" {
		return err
	}

	_, err := conn.Pipelined(ctx, func(pipe Pipeliner) error {
		if !auth && password != "" {
			if username != "" {
				pipe.AuthACL(ctx, username, password)
			} else {
				pipe.Auth(ctx, password)
			}
		}

		if c.opt.DB > 0 {
			pipe.Select(ctx, c.opt.DB)
		}

		if c.opt.readOnly {
			pipe.ReadOnly(ctx)
		}

		return nil
	})
	if err != nil {
		return err
	}

	if c.opt.OnConnect != nil {
		return c.opt.OnConnect(ctx, conn)
	}
	return nil
}

func (c *baseClient) releaseConn(ctx context.Context, cn *pool.Conn, err error) {
	if c.opt.Limiter != nil {
		c.opt.Limiter.ReportResult(err)
	}

	if isBadConn(err, false, c.opt.Addr) {
		c.connPool.Remove(ctx, cn, err)
	} else {
		c.connPool.Put(ctx, cn)
	}
}

func (c *baseClient) withConn(
	ctx context.Context, fn func(context.Context, *pool.Conn) error,
) error {
	// 从连接池获取一个链接
	cn, err := c.getConn(ctx)
	if err != nil {
		return err
	}

	defer func() {
		// 操作结束，把链接重新放回连接池
		c.releaseConn(ctx, cn, err)
	}()

	done := ctx.Done() //nolint:ifshort

	// 判断是否配置了 超时时间，没有则直接执行 fn
	if done == nil {
		err = fn(ctx, cn)
		return err
	}

	// 异步执行，当 ctx 完成后，则关闭连接
	// todo: 不是很清晰
	errc := make(chan error, 1)
	go func() { errc <- fn(ctx, cn) }()

	select {
	case <-done:
		_ = cn.Close()
		// Wait for the goroutine to finish and send something.
		<-errc

		err = ctx.Err()
		return err
	case err = <-errc:
		return err
	}
}

func (c *baseClient) process(ctx context.Context, cmd Cmder) error {
	var lastErr error
	// 根据选项中的 MaxRetries 字段，会做重试操作
	for attempt := 0; attempt <= c.opt.MaxRetries; attempt++ {
		attempt := attempt

		retry, err := c._process(ctx, cmd, attempt)
		if err == nil || !retry {
			return err
		}

		lastErr = err
	}
	return lastErr
}

func (c *baseClient) _process(ctx context.Context, cmd Cmder, attempt int) (bool, error) {
	if attempt > 0 {
		// 结合 ctx 中设置的超时、回退超时，进行休眠
		// 回退超时: 根据重试次数，进行计算超时时间
		// 如果直接超时则直接返回
		if err := internal.Sleep(ctx, c.retryBackoff(attempt)); err != nil {
			return false, err
		}
	}

	retryTimeout := uint32(1)

	// 进行计算
	// withConn 包含了获取链接、释放链接的一些方法
	// func 参数是获得到 链接 后具体执行的方法
	err := c.withConn(ctx, func(ctx context.Context, cn *pool.Conn) error {
		err := cn.WithWriter(ctx, c.opt.WriteTimeout, func(wr *proto.Writer) error {
			// 对链接的具体操作，其中 wr 就是使用 proto 封装的链接
			// 具体内部就是 Redis 协议了
			return writeCmd(wr, cmd)
		})
		if err != nil {
			return err
		}

		// 读取响应，由于数据类型、命令等不同，所有不同的 cmd 类型
		// 有自己特有的读取方式，所以需要看 cmd 的 readReply 方法
		err = cn.WithReader(ctx, c.cmdTimeout(cmd), cmd.readReply)
		if err != nil {
			if cmd.readTimeout() == nil {
				atomic.StoreUint32(&retryTimeout, 1)
			}
			return err
		}

		return nil
	})
	if err == nil {
		return false, nil
	}

	retry := shouldRetry(err, atomic.LoadUint32(&retryTimeout) == 1)
	return retry, err
}

func (c *baseClient) retryBackoff(attempt int) time.Duration {
	return internal.RetryBackoff(attempt, c.opt.MinRetryBackoff, c.opt.MaxRetryBackoff)
}

func (c *baseClient) cmdTimeout(cmd Cmder) time.Duration {
	if timeout := cmd.readTimeout(); timeout != nil {
		t := *timeout
		if t == 0 {
			return 0
		}
		return t + 10*time.Second
	}
	return c.opt.ReadTimeout
}

// Close closes the client, releasing any open resources.
//
// It is rare to Close a Client, as the Client is meant to be
// long-lived and shared between many goroutines.
func (c *baseClient) Close() error {
	var firstErr error
	if c.onClose != nil {
		if err := c.onClose(); err != nil {
			firstErr = err
		}
	}
	if err := c.connPool.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func (c *baseClient) getAddr() string {
	return c.opt.Addr
}

func (c *baseClient) processPipeline(ctx context.Context, cmds []Cmder) error {
	return c.generalProcessPipeline(ctx, cmds, c.pipelineProcessCmds)
}

func (c *baseClient) processTxPipeline(ctx context.Context, cmds []Cmder) error {
	return c.generalProcessPipeline(ctx, cmds, c.txPipelineProcessCmds)
}

type pipelineProcessor func(context.Context, *pool.Conn, []Cmder) (bool, error)

func (c *baseClient) generalProcessPipeline(
	ctx context.Context, cmds []Cmder, p pipelineProcessor,
) error {
	err := c._generalProcessPipeline(ctx, cmds, p)
	if err != nil {
		setCmdsErr(cmds, err)
		return err
	}
	return cmdsFirstErr(cmds)
}

func (c *baseClient) _generalProcessPipeline(
	ctx context.Context, cmds []Cmder, p pipelineProcessor,
) error {
	var lastErr error
	for attempt := 0; attempt <= c.opt.MaxRetries; attempt++ {
		if attempt > 0 {
			if err := internal.Sleep(ctx, c.retryBackoff(attempt)); err != nil {
				return err
			}
		}

		var canRetry bool
		lastErr = c.withConn(ctx, func(ctx context.Context, cn *pool.Conn) error {
			var err error
			canRetry, err = p(ctx, cn, cmds)
			return err
		})
		if lastErr == nil || !canRetry || !shouldRetry(lastErr, true) {
			return lastErr
		}
	}
	return lastErr
}

func (c *baseClient) pipelineProcessCmds(
	ctx context.Context, cn *pool.Conn, cmds []Cmder,
) (bool, error) {
	err := cn.WithWriter(ctx, c.opt.WriteTimeout, func(wr *proto.Writer) error {
		return writeCmds(wr, cmds)
	})
	if err != nil {
		return true, err
	}

	err = cn.WithReader(ctx, c.opt.ReadTimeout, func(rd *proto.Reader) error {
		return pipelineReadCmds(rd, cmds)
	})
	return true, err
}

func pipelineReadCmds(rd *proto.Reader, cmds []Cmder) error {
	for _, cmd := range cmds {
		err := cmd.readReply(rd)
		cmd.SetErr(err)
		if err != nil && !isRedisError(err) {
			return err
		}
	}
	return nil
}

func (c *baseClient) txPipelineProcessCmds(
	ctx context.Context, cn *pool.Conn, cmds []Cmder,
) (bool, error) {
	err := cn.WithWriter(ctx, c.opt.WriteTimeout, func(wr *proto.Writer) error {
		return writeCmds(wr, cmds)
	})
	if err != nil {
		return true, err
	}

	err = cn.WithReader(ctx, c.opt.ReadTimeout, func(rd *proto.Reader) error {
		statusCmd := cmds[0].(*StatusCmd)
		// Trim multi and exec.
		cmds = cmds[1 : len(cmds)-1]

		err := txPipelineReadQueued(rd, statusCmd, cmds)
		if err != nil {
			return err
		}

		return pipelineReadCmds(rd, cmds)
	})
	return false, err
}

func wrapMultiExec(ctx context.Context, cmds []Cmder) []Cmder {
	if len(cmds) == 0 {
		panic("not reached")
	}
	cmdCopy := make([]Cmder, len(cmds)+2)
	cmdCopy[0] = NewStatusCmd(ctx, "multi")
	copy(cmdCopy[1:], cmds)
	cmdCopy[len(cmdCopy)-1] = NewSliceCmd(ctx, "exec")
	return cmdCopy
}

func txPipelineReadQueued(rd *proto.Reader, statusCmd *StatusCmd, cmds []Cmder) error {
	// Parse +OK.
	if err := statusCmd.readReply(rd); err != nil {
		return err
	}

	// Parse +QUEUED.
	for range cmds {
		if err := statusCmd.readReply(rd); err != nil && !isRedisError(err) {
			return err
		}
	}

	// Parse number of replies.
	line, err := rd.ReadLine()
	if err != nil {
		if err == Nil {
			err = TxFailedErr
		}
		return err
	}

	if line[0] != proto.RespArray {
		return fmt.Errorf("redis: expected '*', but got line %q", line)
	}

	return nil
}

// ------------------------------------------------------------------------------

// Client is a Redis client representing a pool of zero or more underlying connections.
// It's safe for concurrent use by multiple goroutines.
//
// Client creates and frees connections automatically; it also maintains a free pool
// of idle connections. You can control the pool size with Config.PoolSize option.
type Client struct {
	*baseClient
	cmdable
	hooks
	ctx context.Context
}

// NewClient returns a client to the Redis Server specified by Options.
func NewClient(opt *Options) *Client {
	// Options 包含所有选项，client、pool
	// 给 Option 添加默认值
	opt.init()

	c := Client{
		// 使用 newConnPool 创建连接池
		baseClient: newBaseClient(opt, newConnPool(opt)),
		ctx:        context.Background(),
	}

	// type cmdable func(ctx context.Context, cmd Cmder) error
	// 函数是第一公民，并设置 c.Process 为具体方法
	// 由于 cmdable 为匿名字段，所有 Client "继承" 了 cmdable 所有方法
	c.cmdable = c.Process

	return &c
}

func (c *Client) clone() *Client {
	clone := *c
	clone.cmdable = clone.Process
	clone.hooks.lock()
	return &clone
}

func (c *Client) WithTimeout(timeout time.Duration) *Client {
	clone := c.clone()
	clone.baseClient = c.baseClient.withTimeout(timeout)
	return clone
}

func (c *Client) Context() context.Context {
	return c.ctx
}

func (c *Client) WithContext(ctx context.Context) *Client {
	if ctx == nil {
		panic("nil context")
	}
	clone := c.clone()
	clone.ctx = ctx
	return clone
}

func (c *Client) Conn(ctx context.Context) *Conn {
	return newConn(ctx, c.opt, pool.NewStickyConnPool(c.connPool))
}

// Do creates a Cmd from the args and processes the cmd.
func (c *Client) Do(ctx context.Context, args ...interface{}) *Cmd {
	cmd := NewCmd(ctx, args...)
	_ = c.Process(ctx, cmd)
	return cmd
}

// Process cmdable 具体调用的方法
func (c *Client) Process(ctx context.Context, cmd Cmder) error {
	return c.hooks.process(ctx, cmd, c.baseClient.process) // c.baseClient.process 具体执行的方法
}

func (c *Client) processPipeline(ctx context.Context, cmds []Cmder) error {
	return c.hooks.processPipeline(ctx, cmds, c.baseClient.processPipeline)
}

func (c *Client) processTxPipeline(ctx context.Context, cmds []Cmder) error {
	return c.hooks.processTxPipeline(ctx, cmds, c.baseClient.processTxPipeline)
}

// Options returns read-only Options that were used to create the client.
func (c *Client) Options() *Options {
	return c.opt
}

type PoolStats pool.Stats

// PoolStats returns connection pool stats.
func (c *Client) PoolStats() *PoolStats {
	stats := c.connPool.Stats()
	return (*PoolStats)(stats)
}

func (c *Client) Pipelined(ctx context.Context, fn func(Pipeliner) error) ([]Cmder, error) {
	return c.Pipeline().Pipelined(ctx, fn)
}

func (c *Client) Pipeline() Pipeliner {
	pipe := Pipeline{
		ctx:  c.ctx,
		exec: c.processPipeline,
	}
	pipe.init()
	return &pipe
}

func (c *Client) TxPipelined(ctx context.Context, fn func(Pipeliner) error) ([]Cmder, error) {
	return c.TxPipeline().Pipelined(ctx, fn)
}

// TxPipeline acts like Pipeline, but wraps queued commands with MULTI/EXEC.
func (c *Client) TxPipeline() Pipeliner {
	pipe := Pipeline{
		ctx:  c.ctx,
		exec: c.processTxPipeline,
	}
	pipe.init()
	return &pipe
}

func (c *Client) pubSub() *PubSub {
	pubsub := &PubSub{
		opt: c.opt,

		newConn: func(ctx context.Context, channels []string) (*pool.Conn, error) {
			return c.newConn(ctx)
		},
		closeConn: c.connPool.CloseConn,
	}
	pubsub.init()
	return pubsub
}

// Subscribe subscribes the client to the specified channels.
// Channels can be omitted to create empty subscription.
// Note that this method does not wait on a response from Redis, so the
// subscription may not be active immediately. To force the connection to wait,
// you may call the Receive() method on the returned *PubSub like so:
//
//    sub := client.Subscribe(queryResp)
//    iface, err := sub.Receive()
//    if err != nil {
//        // handle error
//    }
//
//    // Should be *Subscription, but others are possible if other actions have been
//    // taken on sub since it was created.
//    switch iface.(type) {
//    case *Subscription:
//        // subscribe succeeded
//    case *Message:
//        // received first message
//    case *Pong:
//        // pong received
//    default:
//        // handle error
//    }
//
//    ch := sub.Channel()
func (c *Client) Subscribe(ctx context.Context, channels ...string) *PubSub {
	pubsub := c.pubSub()
	if len(channels) > 0 {
		_ = pubsub.Subscribe(ctx, channels...)
	}
	return pubsub
}

// PSubscribe subscribes the client to the given patterns.
// Patterns can be omitted to create empty subscription.
func (c *Client) PSubscribe(ctx context.Context, channels ...string) *PubSub {
	pubsub := c.pubSub()
	if len(channels) > 0 {
		_ = pubsub.PSubscribe(ctx, channels...)
	}
	return pubsub
}

// ------------------------------------------------------------------------------

type conn struct {
	baseClient
	cmdable
	statefulCmdable
	hooks // TODO: inherit hooks
}

// Conn represents a single Redis connection rather than a pool of connections.
// Prefer running commands from Client unless there is a specific need
// for a continuous single Redis connection.
type Conn struct {
	*conn
	ctx context.Context
}

func newConn(ctx context.Context, opt *Options, connPool pool.Pooler) *Conn {
	c := Conn{
		conn: &conn{
			baseClient: baseClient{
				opt:      opt,
				connPool: connPool,
			},
		},
		ctx: ctx,
	}
	c.cmdable = c.Process
	c.statefulCmdable = c.Process
	return &c
}

func (c *Conn) Process(ctx context.Context, cmd Cmder) error {
	return c.hooks.process(ctx, cmd, c.baseClient.process)
}

func (c *Conn) processPipeline(ctx context.Context, cmds []Cmder) error {
	return c.hooks.processPipeline(ctx, cmds, c.baseClient.processPipeline)
}

func (c *Conn) processTxPipeline(ctx context.Context, cmds []Cmder) error {
	return c.hooks.processTxPipeline(ctx, cmds, c.baseClient.processTxPipeline)
}

func (c *Conn) Pipelined(ctx context.Context, fn func(Pipeliner) error) ([]Cmder, error) {
	return c.Pipeline().Pipelined(ctx, fn)
}

func (c *Conn) Pipeline() Pipeliner {
	pipe := Pipeline{
		ctx:  c.ctx,
		exec: c.processPipeline,
	}
	pipe.init()
	return &pipe
}

func (c *Conn) TxPipelined(ctx context.Context, fn func(Pipeliner) error) ([]Cmder, error) {
	return c.TxPipeline().Pipelined(ctx, fn)
}

// TxPipeline acts like Pipeline, but wraps queued commands with MULTI/EXEC.
func (c *Conn) TxPipeline() Pipeliner {
	pipe := Pipeline{
		ctx:  c.ctx,
		exec: c.processTxPipeline,
	}
	pipe.init()
	return &pipe
}
