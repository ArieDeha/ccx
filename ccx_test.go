package ccx_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	ccx "github.com/ArieDeha/ccx"
	policy "github.com/ArieDeha/ccxpolicy"
)

func TestBackgroundAndTODO(t *testing.T) {
	a := ccx.Background()
	b := ccx.TODO()

	if a == nil || b == nil {
		t.Fatal("expected non-nil contexts")
	}
	if a.State() != "active" || b.State() != "active" {
		t.Fatalf("expected active states, got: %s, %s", a.State(), b.State())
	}
}

func TestWithIntentLineageAndClamp(t *testing.T) {
	root := ccx.Background()
	dl := time.Now().Add(100 * time.Millisecond)
	child, cancel := ccx.WithIntent(root, ccx.Intent{Name: "Child"}, ccx.Constraints{Deadline: dl})
	defer cancel()

	if child.Parent() != root {
		t.Fatal("expected parent link")
	}
	// Child of child trying to exceed deadline should be clamped to parent's deadline.
	grand, cancel2 := ccx.WithIntent(child, ccx.Intent{Name: "Grand"}, ccx.Constraints{Deadline: dl.Add(5 * time.Second)})
	defer cancel2()
	if !grand.Constraints().Deadline.Equal(child.Constraints().Deadline) {
		t.Fatal("expected deadline clamped to parent")
	}
}

func TestSendCancelScopes(t *testing.T) {
	root := ccx.Background()
	a, _ := ccx.WithIntent(root, ccx.Intent{Name: "A"}, ccx.Constraints{})
	b, _ := ccx.WithIntent(a, ccx.Intent{Name: "B"}, ccx.Constraints{})

	// Cancel subtree at A should abort B but not root immediately.
	a.SendCancel(ccx.ScopeSubtree, errors.New("boom"))

	select {
	case <-b.DoneChan():
		// ok
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected B to be canceled quickly")
	}
	if b.State() != "aborted" {
		t.Fatalf("expected B aborted, got %s", b.State())
	}
	if root.State() != "active" {
		t.Fatalf("root should remain active, got %s", root.State())
	}
}

func TestSendAdjustScopes(t *testing.T) {
	root := ccx.Background()
	a, _ := ccx.WithIntent(root, ccx.Intent{Name: "A", Params: map[string]any{}}, ccx.Constraints{})
	b, _ := ccx.WithIntent(a, ccx.Intent{Name: "B", Params: map[string]any{}}, ccx.Constraints{})

	a.SendAdjust(ccx.ScopeSubtree, func(p map[string]any) { p["k"] = "v" })

	if a.Intent().Params["k"] != "v" || b.Intent().Params["k"] != "v" {
		t.Fatal("expected subtree adjustment to set k=v on both A and B")
	}
}

func TestWaitAllAndWhenAny(t *testing.T) {
	root := ccx.Background()
	n1, _ := ccx.WithIntent(root, ccx.Intent{Name: "N1"}, ccx.Constraints{})
	n2, _ := ccx.WithIntent(root, ccx.Intent{Name: "N2"}, ccx.Constraints{})

	go func() { time.Sleep(20 * time.Millisecond); n1.Fulfill() }()
	go func() { time.Sleep(40 * time.Millisecond); n2.Fulfill() }()

	first, err := ccx.WhenAny(context.Background(), n1, n2)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if first != n1 {
		t.Fatal("expected n1 to finish first")
	}
	if err := ccx.WaitAll(context.Background(), n1, n2); err != nil {
		t.Fatalf("wait all failed: %v", err)
	}
}

//
// Policy integration tests
//

// SafetyStop cancels root when safety.block == true.
type SafetyStop struct{}

func (SafetyStop) ID() string               { return "safety_stop" }
func (SafetyStop) Priority() int            { return 1 }
func (SafetyStop) Match(_ policy.Node) bool { return true }
func (SafetyStop) Check(n policy.Node) []policy.Decision {
	if v, ok := n.Params()["safety.block"].(bool); ok && v {
		return []policy.Decision{{
			PolicyID: "safety_stop",
			Scope:    policy.ScopeRoot,
			Action:   policy.ActionCancelRoot,
			Reason:   policy.Reason("blocked"),
			Stop:     true,
		}}
	}
	return nil
}

// QualityCap adjusts transcode.targetQuality down to 1080 if higher.
type QualityCap struct{}

func (QualityCap) ID() string               { return "cap_quality" }
func (QualityCap) Priority() int            { return 5 }
func (QualityCap) Match(n policy.Node) bool { return n.Name() == "Transcode" }
func (QualityCap) Check(n policy.Node) []policy.Decision {
	q, _ := n.Params()["transcode.targetQuality"].(int)
	if q > 1080 {
		return []policy.Decision{{
			PolicyID: "cap_quality",
			Scope:    policy.ScopeSubtree,
			Action:   policy.ActionAdjust,
			Adjust:   func(m map[string]any) { m["transcode.targetQuality"] = 1080 },
			Reason:   policy.Reason("cap 1080"),
		}}
	}
	return nil
}

func TestPolicyEvaluateAndEnforce_AdjustAndCancel(t *testing.T) {
	// Register policies once (process-global).
	policy.RegisterPolicy(SafetyStop{})
	policy.RegisterPolicy(QualityCap{})

	root := ccx.Background()

	// Case 1: quality cap adjustment
	tx, _ := ccx.WithIntent(root, ccx.Intent{
		Name: "Transcode",
		Params: map[string]any{
			"transcode.targetQuality": 1440,
		},
	}, ccx.Constraints{})

	ds := ccx.EvaluatePolicies(tx)
	if len(ds) == 0 {
		t.Fatal("expected at least one decision from QualityCap")
	}
	ccx.EnforcePolicies(tx, ds)
	if got := tx.Intent().Params["transcode.targetQuality"].(int); got != 1080 {
		t.Fatalf("expected quality adjusted to 1080, got %d", got)
	}

	// Case 2: safety stop cancels root
	root2 := ccx.Background()
	node, _ := ccx.WithIntent(root2, ccx.Intent{
		Name:   "Anything",
		Params: map[string]any{"safety.block": true},
	}, ccx.Constraints{})

	ds2 := ccx.EvaluatePolicies(node)
	ccx.EnforcePolicies(node, ds2)

	select {
	case <-root2.DoneChan():
		// ok, canceled
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected root to be canceled by SafetyStop")
	}
	if root2.State() != "aborted" {
		t.Fatalf("expected root aborted, got %s", root2.State())
	}
}

// --- Examples ---

// ExampleBackground shows constructing a root ccx context.
func ExampleBackground() {
	ctx := ccx.Background()
	fmt.Println(ctx.State())
	// Output: active
}

// ExampleWithIntent shows deriving a child with a deadline and fulfilling it.
func ExampleWithIntent() {
	parent := ccx.Background()
	cons := ccx.Constraints{Deadline: time.Now().Add(50 * time.Millisecond)}
	child, cancel := ccx.WithIntent(parent, ccx.Intent{Name: "Task"}, cons)
	defer cancel()

	child.Fulfill()
	<-child.DoneChan()
	fmt.Println(child.State())
	// Output: done
}

// ExampleCtx_SendAdjust demonstrates subtree parameter adjustment.
func ExampleCtx_SendAdjust() {
	root := ccx.Background()
	a, _ := ccx.WithIntent(root, ccx.Intent{Name: "A", Params: map[string]any{}}, ccx.Constraints{})
	b, _ := ccx.WithIntent(a, ccx.Intent{Name: "B", Params: map[string]any{}}, ccx.Constraints{})

	a.SendAdjust(ccx.ScopeSubtree, func(p map[string]any) { p["k"] = "v" })
	fmt.Println(a.Intent().Params["k"], b.Intent().Params["k"])
	// Output: v v
}

// ExampleWaitAll demonstrates waiting on multiple nodes.
func ExampleWaitAll() {
	root := ccx.Background()
	n1, _ := ccx.WithIntent(root, ccx.Intent{Name: "N1"}, ccx.Constraints{})
	n2, _ := ccx.WithIntent(root, ccx.Intent{Name: "N2"}, ccx.Constraints{})

	go func() { time.Sleep(10 * time.Millisecond); n1.Fulfill() }()
	go func() { time.Sleep(15 * time.Millisecond); n2.Fulfill() }()

	_ = ccx.WaitAll(context.Background(), n1, n2)
	fmt.Println("ok")
	// Output: ok
}
