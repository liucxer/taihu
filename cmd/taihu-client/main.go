// Command taihu-client 是 taihu 远程访问层的命令行工具（设计文档_v3 §7）。
// 子命令：put / get / delete / stat，get 支持 off/size 区间读（缺省全量）。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/liucxer/taihu/internal/version"
	"github.com/liucxer/taihu/pkg/rpcclient"
)

func main() {
	if len(os.Args) >= 2 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Printf("taihu-client %s\n", version.String())
		return
	}
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	rest := os.Args[2:]

	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	addr := fs.String("addr", ":50051", "taihu-server address")
	key := fs.String("key", "", "object key")
	var (
		file = fs.String("file", "", "input/output file (absent: stdin/stdout)")
		size = fs.Int64("size", -1, "get: read length (-1=to end); put: optional, defaults to input length")
		off  = fs.Int64("off", 0, "get: start offset")
	)
	if err := fs.Parse(rest); err != nil {
		os.Exit(2)
	}

	ctx := context.Background()
	s, err := rpcclient.Dial(ctx, *addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial %s: %v\n", *addr, err)
		os.Exit(1)
	}
	defer s.Close()

	switch cmd {
	case "put":
		doPut(ctx, s, *key, *size, *file)
	case "get":
		doGet(ctx, s, *key, *off, *size, *file)
	case "delete":
		doDelete(ctx, s, *key)
	case "stat":
		doStat(ctx, s, *key)
	default:
		usage()
		os.Exit(2)
	}
}

func doPut(ctx context.Context, s *rpcclient.Storage, key string, size int64, file string) {
	var in io.Reader
	if file != "" {
		f, err := os.Open(file)
		must(err)
		defer f.Close()
		in = f
	} else {
		in = os.Stdin
	}
	data, err := io.ReadAll(in)
	must(err)
	if size < 0 || size > int64(len(data)) {
		size = int64(len(data))
	}
	if err := s.Put(ctx, key, size, data); err != nil {
		fmt.Fprintf(os.Stderr, "put %q: %v\n", key, err)
		os.Exit(1)
	}
	fmt.Printf("put %q ok (%d bytes)\n", key, size)
}

func doGet(ctx context.Context, s *rpcclient.Storage, key string, off, size int64, file string) {
	data, rel, err := s.Get(ctx, key, off, size)
	if err != nil {
		fmt.Fprintf(os.Stderr, "get %q: %v\n", key, err)
		os.Exit(1)
	}
	if data != nil {
		defer rel() // Get 返回私有缓冲，用毕归还
	}

	var out io.Writer
	if file != "" {
		f, err := os.Create(file)
		must(err)
		defer f.Close()
		out = f
	} else {
		out = os.Stdout
	}
	n, werr := out.Write(data)
	must(werr)
	fmt.Fprintf(os.Stderr, "get %q: %d bytes\n", key, n)
}

func doDelete(ctx context.Context, s *rpcclient.Storage, key string) {
	if err := s.Delete(ctx, key); err != nil {
		fmt.Fprintf(os.Stderr, "delete %q: %v\n", key, err)
		os.Exit(1)
	}
	fmt.Printf("delete %q ok\n", key)
}

func doStat(ctx context.Context, s *rpcclient.Storage, key string) {
	sz, err := s.Stat(ctx, key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stat %q: %v\n", key, err)
		os.Exit(1)
	}
	fmt.Printf("stat %q: size=%d\n", key, sz)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: taihu-client -version | <put|get|delete|stat> -addr :50051 [flags]
  -version              显示版本号（commit_日期，如 8abdf4d_202609111002）
  put    -key K -size N [-file F]    上传对象（缺省 stdin）
  get    -key K [-off O] [-size N] [-file F]  下载（size=-1 全量，缺省 stdout）
  delete -key K
  stat   -key K`)
}
