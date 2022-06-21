package main

import (
	"context"

	"github.com/go-redis/redis/v9"
)

func main() {
	// 源代码入口 1
	rdb := redis.NewClient(&redis.Options{
		Addr: ":6379",
	})

	// 源代码入口 2
	_ = rdb.Get(context.Background(), "key").String()
}
