package rpc_test

import (
	"context"
	"testing"

	"github.com/godwit-email/godwit/rpc"
)

type callKey struct{}

type callAPI struct{ ended chan context.Context }

func (a *callAPI) Probe(ctx context.Context) string {
	v, _ := ctx.Value(callKey{}).(string)
	a.ended <- ctx
	return v
}

// CallContext sees each call's role, method and lane hint, and wraps the
// call's own ctx, which ends when the call returns.
func TestCallContextAndLaneHint(t *testing.T) {
	dir := shortDir(t)
	a := &callAPI{ended: make(chan context.Context, 4)}
	d, err := rpc.NewDispatcher(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := rpc.Listen(dir, d, rpc.ServerOptions{Version: "test", CallContext: func(ctx context.Context, role, method, hint string) context.Context {
		return context.WithValue(ctx, callKey{}, role+"/"+method+"/"+hint)
	}})
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(context.Background())
	t.Cleanup(func() { s.Close() })

	ui, err := rpc.Dial(dir, rpc.RoleUI, "t", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ui.Close()
	for _, tc := range []struct {
		ctx  context.Context
		want string
	}{
		{context.Background(), "ui/Probe/"},
		{rpc.WithLaneHint(context.Background(), "ui-background"), "ui/Probe/ui-background"},
	} {
		var got string
		if err := ui.Call(tc.ctx, "Probe", &got); err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Fatalf("Probe = %q, want %q", got, tc.want)
		}
		callCtx := <-a.ended
		<-callCtx.Done() // the call's ctx ends with the call
	}

	full, err := rpc.Dial(dir, rpc.RoleFull, "t", nil)
	if err == nil {
		defer full.Close()
		var got string
		if err := full.Call(context.Background(), "Probe", &got); err != nil {
			t.Fatal(err)
		}
		if got != "full/Probe/" {
			t.Fatalf("full Probe = %q", got)
		}
		<-a.ended
	}
}
