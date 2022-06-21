package pool

import (
	"bufio"
	"context"
	"net"
	"sync/atomic"
	"time"

	"github.com/go-redis/redis/v9/internal/proto"
)

var noDeadline = time.Time{}

type Conn struct {
	usedAt  int64 // atomic
	netConn net.Conn

	rd *proto.Reader
	bw *bufio.Writer
	wr *proto.Writer

	Inited    bool
	pooled    bool
	createdAt time.Time
}

func NewConn(netConn net.Conn) *Conn {
	cn := &Conn{
		netConn:   netConn,
		createdAt: time.Now(),
	}
	cn.rd = proto.NewReader(netConn) // 读取 TCP 方法的封装
	cn.bw = bufio.NewWriter(netConn) // writer 增加 bufio
	cn.wr = proto.NewWriter(cn.bw)   // 写入 TCP 时的协议封装
	cn.SetUsedAt(time.Now())         // 链接的创建时间
	return cn
}

func (cn *Conn) UsedAt() time.Time {
	unix := atomic.LoadInt64(&cn.usedAt)
	return time.Unix(unix, 0)
}

func (cn *Conn) SetUsedAt(tm time.Time) {
	atomic.StoreInt64(&cn.usedAt, tm.Unix())
}

func (cn *Conn) SetNetConn(netConn net.Conn) {
	cn.netConn = netConn
	cn.rd.Reset(netConn)
	cn.bw.Reset(netConn)
}

func (cn *Conn) Write(b []byte) (int, error) {
	return cn.netConn.Write(b)
}

func (cn *Conn) RemoteAddr() net.Addr {
	if cn.netConn != nil {
		return cn.netConn.RemoteAddr()
	}
	return nil
}

func (cn *Conn) WithReader(ctx context.Context, timeout time.Duration, fn func(rd *proto.Reader) error) error {
	if timeout != 0 {
		if err := cn.netConn.SetReadDeadline(cn.deadline(ctx, timeout)); err != nil {
			return err
		}
	}
	return fn(cn.rd)
}

func (cn *Conn) WithWriter(
	ctx context.Context, timeout time.Duration, fn func(wr *proto.Writer) error,
) error {
	if timeout != 0 {
		// 如果配置了 Write 超时，则配置 TCP 的超时时间
		// 嘿嘿嘿，这个说明之前「通道」的写法没有错误
		if err := cn.netConn.SetWriteDeadline(cn.deadline(ctx, timeout)); err != nil {
			return err
		}
	}

	// 判断链接的缓冲区（应用层，bufio）还有之前的数据
	// 则会清除之前的数据
	if cn.bw.Buffered() > 0 {
		cn.bw.Reset(cn.netConn)
	}

	if err := fn(cn.wr); err != nil {
		return err
	}

	// TCP 的操作都是封装了，write 方法使用 bufio 封装了
	// 所以需要写入到
	return cn.bw.Flush()
}

func (cn *Conn) Close() error {
	return cn.netConn.Close()
}

func (cn *Conn) deadline(ctx context.Context, timeout time.Duration) time.Time {
	tm := time.Now()
	cn.SetUsedAt(tm)

	if timeout > 0 {
		tm = tm.Add(timeout)
	}

	if ctx != nil {
		deadline, ok := ctx.Deadline()
		if ok {
			if timeout == 0 {
				return deadline
			}
			if deadline.Before(tm) {
				return deadline
			}
			return tm
		}
	}

	if timeout > 0 {
		return tm
	}

	return noDeadline
}
