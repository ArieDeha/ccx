// Copyright 2025 Arieditya Pramadyana Deha <arieditya.prdh@live.com>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package ccx provides a stdlib-only cascading context with intent, lineage,
// and scoped control. It integrates with the separate ccxpolicy module via a
// small adapter (see EvaluatePolicies / EnforcePolicies).
package ccx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	policy "github.com/ArieDeha/ccxpolicy"
)

// IntentName is a stable identifier for a business/operational intent.
type IntentName string

// Intent carries the purpose and small parameter map.
type Intent struct {
	Name   IntentName
	Params map[string]any
}

// Constraints keeps only universal limits (domain-neutral).
type Constraints struct {
	Deadline time.Time
}

// AdjustFn mutates Intent.Params for a node.
type AdjustFn func(params map[string]any)

// Scope defines where a cancel/adjust applies.
type Scope int

const (
	ScopeNode Scope = iota
	ScopeSubtree
	ScopeRoot
)

// Ctx is a cascading context that embeds context.Context and adds metadata.
type Ctx struct {
	context.Context
	id, parentID string
	intent       Intent
	cons         Constraints

	mu       sync.RWMutex
	state    string // "active"|"done"|"aborted"
	doneCh   chan struct{}
	err      error
	children []string
}

var _ context.Context = (*Ctx)(nil)

// Background returns a root ccx context based on context.Background().
func Background() *Ctx { return wrap(context.Background(), Intent{Name: ""}, Constraints{}) }

// TODO returns a root ccx context based on context.TODO().
func TODO() *Ctx { return wrap(context.TODO(), Intent{Name: ""}, Constraints{}) }

func wrap(base context.Context, intent Intent, cons Constraints) *Ctx {
	c := &Ctx{Context: base, id: newID(), intent: intent, cons: cons, state: "active", doneCh: make(chan struct{})}
	register(c)
	return c
}

// Accessors

// ID returns the unique identifier of this node.
func (c *Ctx) ID() string { return c.id }

// ParentID returns the identifier of the parent node or empty for root.
func (c *Ctx) ParentID() string { return c.parentID }

// Intent returns a snapshot of the node's Intent.
func (c *Ctx) Intent() Intent { c.mu.RLock(); defer c.mu.RUnlock(); return c.intent }

// Constraints returns the node's Constraints.
func (c *Ctx) Constraints() Constraints { return c.cons }

// State returns the lifecycle state: "active", "done", or "aborted".
func (c *Ctx) State() string { c.mu.RLock(); defer c.mu.RUnlock(); return c.state }

// DoneChan is closed when the node finishes (Fulfill/Abort).
func (c *Ctx) DoneChan() <-chan struct{} { return c.doneCh }

// ErrState returns the abort error if the node aborted, or nil otherwise.
func (c *Ctx) ErrState() error { c.mu.RLock(); defer c.mu.RUnlock(); return c.err }

// Lifecycle

// Fulfill marks the node as successfully completed and closes DoneChan.
func (c *Ctx) Fulfill() {
	c.mu.Lock()
	if c.state == "active" {
		c.state = "done"
		close(c.doneCh)
	}
	c.mu.Unlock()
}

// Abort marks the node as aborted with an error and closes DoneChan.
func (c *Ctx) Abort(err error) {
	c.mu.Lock()
	if c.state == "active" {
		c.state = "aborted"
		c.err = err
		close(c.doneCh)
	}
	c.mu.Unlock()
}

// Lineage registry (in-process)

var reg sync.Map // id -> *Ctx

func register(c *Ctx) { reg.Store(c.id, c) }
func get(id string) (*Ctx, bool) {
	if v, ok := reg.Load(id); ok {
		return v.(*Ctx), true
	}
	return nil, false
}

// Parent returns the parent node, or nil if this is root.
func (c *Ctx) Parent() *Ctx { p, _ := get(c.parentID); return p }

// Root returns the root ancestor for this node.
func (c *Ctx) Root() *Ctx {
	cur := c
	for cur.parentID != "" {
		cur = cur.Parent()
	}
	return cur
}

// Children returns the direct children of this node.
func (c *Ctx) Children() []*Ctx {
	c.mu.RLock()
	ids := append([]string(nil), c.children...)
	c.mu.RUnlock()
	out := make([]*Ctx, 0, len(ids))
	for _, id := range ids {
		if ch, ok := get(id); ok {
			out = append(out, ch)
		}
	}
	return out
}

// WithIntent derives a child context from parent with the given intent and
// constraints. Deadlines clamp downward; if the child specifies a later
// deadline than the parent, the parent's deadline is used.
func WithIntent(parent *Ctx, intent Intent, cons Constraints) (*Ctx, context.CancelFunc) {
	cons = clampChild(parent.cons, cons)
	var base context.Context
	var cancel context.CancelFunc
	if cons.Deadline.IsZero() {
		base, cancel = context.WithCancel(parent.Context)
	} else {
		base, cancel = context.WithDeadline(parent.Context, cons.Deadline)
	}
	child := wrap(base, intent, cons)
	child.parentID = parent.id
	parent.mu.Lock()
	parent.children = append(parent.children, child.id)
	parent.mu.Unlock()
	return child, cancel
}

func clampChild(p Constraints, c Constraints) Constraints {
	out := c
	if !p.Deadline.IsZero() && (out.Deadline.IsZero() || out.Deadline.After(p.Deadline)) {
		out.Deadline = p.Deadline
	}
	return out
}

// Scoped controls

// SendCancel applies a cancellation to a node at the specified scope (Node,
// Subtree, or Root).
func (c *Ctx) SendCancel(scope Scope, reason error) {
	switch scope {
	case ScopeNode:
		c.Abort(reason)
	case ScopeSubtree:
		c.abortRecursive(reason)
	case ScopeRoot:
		c.Root().abortRecursive(reason)
	}
}

func (c *Ctx) abortRecursive(reason error) {
	if c.State() == "active" {
		c.Abort(reason)
	}
	for _, ch := range c.Children() {
		ch.abortRecursive(reason)
	}
}

// SendAdjust applies an AdjustFn to Intent.Params at the specified scope. In
// this minimal, in-process implementation, adjustments are last-writer-wins;
// prefer idempotent updates and namespaced keys.
func (c *Ctx) SendAdjust(scope Scope, fn AdjustFn) {
	switch scope {
	case ScopeNode:
		c.applyAdjust(fn)
	case ScopeSubtree:
		c.adjustRecursive(fn)
	case ScopeRoot:
		c.Root().adjustRecursive(fn)
	}
}

func (c *Ctx) applyAdjust(fn AdjustFn) {
	c.mu.Lock()
	if c.intent.Params == nil {
		c.intent.Params = map[string]any{}
	}
	fn(c.intent.Params)
	c.mu.Unlock()
}

func (c *Ctx) adjustRecursive(fn AdjustFn) {
	c.applyAdjust(fn)
	for _, ch := range c.Children() {
		ch.adjustRecursive(fn)
	}
}

// Wait helpers

// WaitAll blocks until all nodes complete or ctx is done. Returns the first
// non-nil node error or ctx error.
func WaitAll(ctx context.Context, nodes ...*Ctx) error {
	for _, n := range nodes {
		select {
		case <-n.DoneChan():
			if err := n.ErrState(); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// WhenAny returns the first node to complete (and its error if any), or ctx
// error if the context finishes first.
func WhenAny(ctx context.Context, nodes ...*Ctx) (*Ctx, error) {
	ch := make(chan *Ctx, len(nodes))
	for _, n := range nodes {
		go func(n *Ctx) { <-n.DoneChan(); ch <- n }(n)
	}
	select {
	case n := <-ch:
		return n, n.ErrState()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ID helper

func newID() string { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }

// ----------------------
// Policy module adapters
// ----------------------

// policyNode adapts *Ctx to ccxpolicy.Node (read-only view).
type policyNode struct{ n *Ctx }

func (p policyNode) ID() string             { return p.n.ID() }
func (p policyNode) Name() string           { return string(p.n.Intent().Name) }
func (p policyNode) Params() map[string]any { return p.n.Intent().Params }
func (p policyNode) Parent() policy.Node {
	if par := p.n.Parent(); par != nil {
		return policyNode{par}
	}
	return nil
}
func (p policyNode) Root() policy.Node { return policyNode{p.n.Root()} }

// policyEnforcer maps decisions back onto *Ctx.
type policyEnforcer struct{ n *Ctx }

func (e policyEnforcer) Adjust(s policy.Scope, fn func(map[string]any)) {
	e.n.SendAdjust(fromPolScope(s), fn)
}
func (e policyEnforcer) Cancel(s policy.Scope, reason error) {
	e.n.SendCancel(fromPolScope(s), reason)
}
func (e policyEnforcer) Warn(_ string, _ error) {
	// no-op; applications can wrap EnforcePolicies to log warnings if desired
}

func fromPolScope(s policy.Scope) Scope {
	switch s {
	case policy.ScopeRoot:
		return ScopeRoot
	case policy.ScopeSubtree:
		return ScopeSubtree
	default:
		return ScopeNode
	}
}

// EvaluatePolicies delegates to the external policy module using the adapter.
func EvaluatePolicies(n *Ctx) []policy.Decision { return policy.Evaluate(policyNode{n}) }

// EnforcePolicies applies decisions onto this node using the external module.
func EnforcePolicies(n *Ctx, ds []policy.Decision) { policy.Enforce(policyEnforcer{n}, ds) }
