// examples/taihu-client/main.go 的用例：run 的完整读写删流程与各步骤失败分支。
// 通过替换 newClusterClient 注入假客户端，全程不连 TiKV/PD。
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/liucxer/taihu/pkg/taihu-client"
)

// cliTestClient 是可编程的假集群客户端。
type cliTestClient struct {
	poolErr   error
	putErr    error
	getErrs   []error // 按 Get 调用顺序返回的错误（nil=成功）
	out       []byte
	getCalls  int
	statErr   error
	statSize  int64
	deleteErr error
	closed    bool
	released  int
}

func (c *cliTestClient) CheckPoolIsValid() error { return c.poolErr }

func (c *cliTestClient) Put(context.Context, string, int64, []byte) error { return c.putErr }

func (c *cliTestClient) Get(context.Context, string, int64, int64) ([]byte, func(), error) {
	i := c.getCalls
	c.getCalls++
	if i < len(c.getErrs) && c.getErrs[i] != nil {
		return nil, nil, c.getErrs[i]
	}
	return c.out, func() { c.released++ }, nil
}

func (c *cliTestClient) Stat(context.Context, string) (int64, error) {
	return c.statSize, c.statErr
}

func (c *cliTestClient) Delete(context.Context, string) error { return c.deleteErr }

func (c *cliTestClient) Close() error {
	c.closed = true
	return nil
}

// cliTestInstall 用假客户端（或构造错误）替换 newClusterClient。
func cliTestInstall(t *testing.T, cli *cliTestClient, err error) {
	t.Helper()
	old := newClusterClient
	newClusterClient = func(context.Context, string) (clusterClient, error) {
		if err != nil {
			return nil, err
		}
		return cli, nil
	}
	t.Cleanup(func() { newClusterClient = old })
}

// cliTestRun 以给定假客户端跑一遍 run，返回输出与错误。
func cliTestRun(t *testing.T, cli *cliTestClient) (string, error) {
	t.Helper()
	cliTestInstall(t, cli, nil)
	var buf bytes.Buffer
	err := run(context.Background(), "127.0.0.1:2379,127.0.0.1:2380", &buf)
	return buf.String(), err
}

func TestCLIRunMissingPD(t *testing.T) {
	cliTestInstall(t, nil, nil)
	var buf bytes.Buffer
	err := run(context.Background(), "", &buf)
	if err == nil || !strings.Contains(err.Error(), "TAIHU_PD 未设置") {
		t.Fatalf("run(空 pd) = %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("空 pd 不应有输出：%q", buf.String())
	}
}

func TestCLIRunConnectError(t *testing.T) {
	cliTestInstall(t, nil, errors.New("pd unreachable"))
	var buf bytes.Buffer
	err := run(context.Background(), "127.0.0.1:2379", &buf)
	if err == nil || !strings.Contains(err.Error(), "连接 taihu 集群失败") {
		t.Fatalf("run(连接失败) = %v", err)
	}
}

func TestCLIRunNoInstances(t *testing.T) {
	out, err := cliTestRun(t, &cliTestClient{poolErr: errors.New("no online instance")})
	if err == nil || !strings.Contains(err.Error(), "集群无在线实例") {
		t.Fatalf("run(无在线实例) = %v, out=%q", err, out)
	}
}

func TestCLIRunPutError(t *testing.T) {
	out, err := cliTestRun(t, &cliTestClient{putErr: errors.New("put boom")})
	if err == nil || !strings.Contains(err.Error(), "Put 失败") {
		t.Fatalf("run(Put 失败) = %v, out=%q", err, out)
	}
}

func TestCLIRunGetError(t *testing.T) {
	out, err := cliTestRun(t, &cliTestClient{getErrs: []error{errors.New("get boom")}})
	if err == nil || !strings.Contains(err.Error(), "Get 失败") {
		t.Fatalf("run(Get 失败) = %v, out=%q", err, out)
	}
}

func TestCLIRunStatError(t *testing.T) {
	out, err := cliTestRun(t, &cliTestClient{
		out:     []byte(`{"a":1}`),
		statErr: errors.New("stat boom"),
	})
	if err == nil || !strings.Contains(err.Error(), "Stat 失败") {
		t.Fatalf("run(Stat 失败) = %v, out=%q", err, out)
	}
}

func TestCLIRunRangeReadError(t *testing.T) {
	out, err := cliTestRun(t, &cliTestClient{
		out:     []byte(`{"a":1}`),
		getErrs: []error{nil, errors.New("range boom")},
	})
	if err == nil || !strings.Contains(err.Error(), "区间读失败") {
		t.Fatalf("run(区间读失败) = %v, out=%q", err, out)
	}
}

func TestCLIRunDeleteError(t *testing.T) {
	out, err := cliTestRun(t, &cliTestClient{
		out:       []byte(`{"a":1}`),
		deleteErr: errors.New("delete boom"),
	})
	if err == nil || !strings.Contains(err.Error(), "Delete 失败") {
		t.Fatalf("run(Delete 失败) = %v, out=%q", err, out)
	}
}

func TestCLIRunAfterDeleteUnexpected(t *testing.T) {
	out, err := cliTestRun(t, &cliTestClient{
		out:     []byte(`{"a":1}`),
		getErrs: []error{nil, nil, errors.New("after boom")},
	})
	if err == nil || !strings.Contains(err.Error(), "删除后 Get 非预期错误") {
		t.Fatalf("run(删除后取回异常) = %v, out=%q", err, out)
	}
}

func TestCLIRunSuccessNotFound(t *testing.T) {
	cli := &cliTestClient{
		out:      []byte(`{"a":1}`),
		getErrs:  []error{nil, nil, taihuclient.ErrNotFound},
		statSize: 14,
	}
	out, err := cliTestRun(t, cli)
	if err != nil {
		t.Fatalf("run(成功) = %v, out=%q", err, out)
	}
	const key = "orders/2026-09-14/001"
	data := []byte(`{"order_id":1,"amount":99.9}`)
	for _, want := range []string{
		fmt.Sprintf("Put %q size=%d", key, len(data)),
		fmt.Sprintf("Get %q -> %s", key, `{"a":1}`),
		fmt.Sprintf("Stat %q size=14", key),
		`Get [0,8) -> {"a":1}`,
		"删除后 Get 预期返回 ErrNotFound",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %q：%q", want, out)
		}
	}
	if !cli.closed {
		t.Fatal("run 结束时应 Close 客户端")
	}
	if cli.released != 2 {
		t.Fatalf("release 调用次数 = %d, want 2（Get 与区间读各一次）", cli.released)
	}
}

// TestCLIRunAfterDeleteNil 覆盖删除后 Get 返回 nil 错误（不打印提示、正常结束）。
func TestCLIRunAfterDeleteNil(t *testing.T) {
	out, err := cliTestRun(t, &cliTestClient{
		out:     []byte(`{"a":1}`),
		getErrs: []error{nil, nil, nil},
	})
	if err != nil {
		t.Fatalf("run(删除后 Get=nil) = %v, out=%q", err, out)
	}
	if strings.Contains(out, "预期返回 ErrNotFound") {
		t.Fatalf("不应打印删除提示：%q", out)
	}
}
