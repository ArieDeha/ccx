# ccx — Cascading Context for Go (stdlib-only)

**ccx** adds *intent, lineage, and scoped control* on top of Go’s `context.Context`, while keeping ergonomics nearly identical. It helps you build multi-step, reactive workflows (3+ levels) with cancellation and parameter adjustments that can flow **down**, **up (root)**, or **laterally** across siblings.

* ✅ **Context-like ergonomics**: `Background()`, `TODO()`, `WithIntent(...)` → `(*ccx.Ctx, context.CancelFunc)`
* ✅ **No dependencies**: stdlib-only
* ✅ **Scoped controls**: cancel/adjust at **Node**, **Subtree**, or **Root**
* ✅ **Lineage helpers**: `Parent()`, `Root()`, `Children()`
* ✅ **Drop-in**: `*ccx.Ctx` embeds `context.Context`

---

## Install

```bash
go get github.com/yourorg/ccx@latest
```

---

## TL;DR Usage

```go
root := ccx.Background()
rootCons := ccx.Constraints{Deadline: time.Now().Add(5*time.Second)}
child, cancel := ccx.WithIntent(
    root,
    ccx.Intent{Name: "Task", Params: map[string]any{"k": "v"}},
    rootCons,
)
defer cancel()

// ... do work ...
child.Fulfill()
```

---

## Using Policies (optional, via separate module)

If you want threshold-style governance (“if params exceed X, then adjust/cancel”), use the **separate** policy module `github.com/yourorg/ccxpolicy`. The **ccx** core stays domain-agnostic; it only provides adapters to *evaluate* and *enforce* policies.

### 1) Install

```bash
go get github.com/yourorg/ccx@latest
go get github.com/yourorg/ccxpolicy@latest
```

### 2) Define and register your policies (in your app)

```go
import (
    ccx "github.com/yourorg/ccx"
    policy "github.com/yourorg/ccxpolicy"
)

// Example: cap transcode quality; if above 1080, adjust to 1080.
type QualityCap struct{}
func (QualityCap) ID() string               { return "cap_quality" }
func (QualityCap) Priority() int            { return 10 }
func (QualityCap) Match(n policy.Node) bool { return n.Name() == "Transcode" }
func (QualityCap) Check(n policy.Node) []policy.Decision {
    q, _ := n.Params()["transcode.targetQuality"].(int)
    if q > 1080 {
        return []policy.Decision{{
            PolicyID: "cap_quality",
            Scope:    policy.ScopeSubtree,
            Action:   policy.ActionAdjust,
            Adjust:   func(p map[string]any){ p["transcode.targetQuality"] = 1080 },
            Reason:   policy.Reason("quality above cap"),
            Stop:     false,
        }}
    }
    return nil
}

func init() { policy.RegisterPolicy(QualityCap{}) }
```

### 3) Evaluate + enforce at runtime

Call these when appropriate (after `WithIntent`, before/after work chunks, or on signals):

```go
node, _ := ccx.WithIntent(
    parent,
    ccx.Intent{
        Name:   "Transcode",
        Params: map[string]any{"transcode.targetQuality": 1440},
    },
    ccx.Constraints{},
)

// Evaluate registered policies against this node
ds := ccx.EvaluatePolicies(node)

// Enforce results on this node (adjust or cancel as decided)
ccx.EnforcePolicies(node, ds)
```

**Notes**

* Policies are **unlimited** and **app-owned**; `ccx` doesn’t define budgets or retries.
* Actions map to `ccx` primitives:

    * `ActionAdjust` → `node.SendAdjust(scope, fn)`
    * `ActionCancel*` → `node.SendCancel(scope, reason)`
* Use policy **Priority** and `Decision.Stop` to control ordering/short-circuiting.

---

## Function-by-Function Examples (core API)

### Construction

#### `Background()`

```go
ctx := ccx.Background() // like context.Background(), but with ccx metadata
```

#### `TODO()`

```go
ctx := ccx.TODO() // like context.TODO(), placeholder parent
```

#### `WithIntent(parent *ccx.Ctx, intent Intent, cons Constraints)`

Creates a **child** that embeds a stdlib context and clamps deadlines to the parent.

```go
parent := ccx.Background()
cons := ccx.Constraints{Deadline: time.Now().Add(2*time.Second)}
child, cancel := ccx.WithIntent(parent, ccx.Intent{
    Name:   "ResizeImage",
    Params: map[string]any{"size": 320},
}, cons)
defer cancel()
```

---

### Lifecycle

#### `(*Ctx).Fulfill()`

```go
if child.State() == "active" {
    child.Fulfill()
}
```

#### `(*Ctx).Abort(err error)`

```go
child.Abort(fmt.Errorf("early exit"))
```

---

### Scopes & Signals

#### `Scope` constants

```go
// ccx.ScopeNode, ccx.ScopeSubtree, ccx.ScopeRoot
```

#### `(*Ctx).SendCancel(scope Scope, reason error)`

```go
// Cancel the entire workflow from a safety checker:
safety.SendCancel(ccx.ScopeRoot, fmt.Errorf("unsafe puddle"))
// Cancel only a branch (subtree)
branch.SendCancel(ccx.ScopeSubtree, context.Canceled)
// Cancel just this node
node.SendCancel(ccx.ScopeNode, context.Canceled)
```

#### `(*Ctx).SendAdjust(scope Scope, fn AdjustFn)`

Mutate `Intent.Params` at a specific scope. Prefer **idempotent** updates and **namespaced keys**.

```go
// Degrade a transcode branch to 720p
transcode.SendAdjust(ccx.ScopeSubtree, func(p map[string]any){
    p["transcode.targetQuality"] = 720
})

// Nudge only one node
stepRight.SendAdjust(ccx.ScopeNode, func(p map[string]any){
    p["foot.right.soleTilt"] = 5.0 // degrees
})

// Apply a global safety switch at root
anyNode.SendAdjust(ccx.ScopeRoot, func(p map[string]any){
    p["policy.safety.block"] = true
})
```

---

### Accessors & Lineage

```go
id       := node.ID()
parentID := node.ParentID()
intent   := node.Intent()         // {Name, Params}
cons     := node.Constraints()    // {Deadline}
state    := node.State()          // "active"|"done"|"aborted"
err      := node.ErrState()       // non-nil if aborted

<-node.DoneChan()                 // wait for completion

parent   := node.Parent()
root     := node.Root()
children := node.Children()
```

---

### Utilities

#### `WaitAll(ctx context.Context, nodes ...*ccx.Ctx) error`

Wait for all nodes; returns first node error or ctx error.

```go
if err := ccx.WaitAll(context.Background(), n1, n2, n3); err != nil {
    log.Println("failed:", err)
}
```

#### `WhenAny(ctx context.Context, nodes ...*ccx.Ctx) (*ccx.Ctx, error)`

Return the first node to complete (and its error if any).

```go
first, err := ccx.WhenAny(context.Background(), n1, n2, n3)
log.Println("first done:", first.ID(), "err:", err)
```

---

## Minimal End-to-End Sketch (10 lines)

> Not a full app—just a taste of orchestration using the pieces above.

```go
root := ccx.Background()
rootCons := ccx.Constraints{Deadline: time.Now().Add(3*time.Second)}
parent, cancel := ccx.WithIntent(root, ccx.Intent{Name: "RootIntent"}, rootCons)
defer cancel()
child1,_ := ccx.WithIntent(parent, ccx.Intent{Name: "Child1"}, ccx.Constraints{})
child2,_ := ccx.WithIntent(parent, ccx.Intent{Name: "Child2"}, ccx.Constraints{})
go func(){ time.Sleep(100*time.Millisecond); child1.Fulfill() }()
go func(){ time.Sleep(150*time.Millisecond); child2.Fulfill() }()
_ = ccx.WaitAll(context.Background(), child1, child2)
parent.Fulfill()
```

---

## Design Notes

* **Zero deps** (stdlib only). No background workers or buses.
* **Thread-safe** parameter adjustments (mutex-protected) and lightweight lineage registry.
* **Deterministic** “last-writer-wins” adjustments; use idempotent, namespaced keys.
* **Interop**: `*ccx.Ctx` embeds `context.Context`, so you can pass it anywhere a `context.Context` is expected.
* **Policies live outside**: pair with `github.com/yourorg/ccxpolicy` when you want declarative thresholds.

---

## License

Apache-2.0 (or your preference).
