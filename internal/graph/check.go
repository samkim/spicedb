package graph

import (
	"context"
	"errors"

	"github.com/rs/zerolog/log"

	"github.com/authzed/spicedb/internal/datastore"
	"github.com/authzed/spicedb/internal/namespace"
	v0 "github.com/authzed/spicedb/pkg/proto/authzed/api/v0"
)

func newConcurrentChecker(d Dispatcher, ds datastore.GraphDatastore, nsm namespace.Manager) checker {
	return &concurrentChecker{d: d, ds: ds, nsm: nsm}
}

type concurrentChecker struct {
	d   Dispatcher
	ds  datastore.GraphDatastore
	nsm namespace.Manager
}

func onrEqual(lhs, rhs *v0.ObjectAndRelation) bool {
	// Properties are sorted by highest to lowest cardinality to optimize for short-circuiting.
	return lhs.ObjectId == rhs.ObjectId && lhs.Relation == rhs.Relation && lhs.Namespace == rhs.Namespace
}

func (cc *concurrentChecker) check(ctx context.Context, req CheckRequest, relation *v0.Relation) ReduceableCheckFunc {
	// If we have found the goal's ONR, then we know that the ONR is a member.
	if onrEqual(req.Goal, req.Start) {
		return AlwaysMember()
	}

	if relation.UsersetRewrite == nil {
		return cc.checkDirect(ctx, req)
	}

	return cc.checkUsersetRewrite(ctx, req, relation.UsersetRewrite)
}

func (cc *concurrentChecker) dispatch(req CheckRequest) ReduceableCheckFunc {
	return func(ctx context.Context, resultChan chan<- CheckResult) {
		log.Trace().Object("dispatch", req).Send()
		result := cc.d.Check(ctx, req)
		resultChan <- result
	}
}

func (cc *concurrentChecker) checkDirect(ctx context.Context, req CheckRequest) ReduceableCheckFunc {
	return func(ctx context.Context, resultChan chan<- CheckResult) {
		log.Trace().Object("direct", req).Send()
		it, err := cc.ds.QueryTuples(req.Start.Namespace, req.AtRevision).
			WithObjectID(req.Start.ObjectId).
			WithRelation(req.Start.Relation).
			Execute(ctx)
		if err != nil {
			resultChan <- CheckResult{false, NewCheckFailureErr(err)}
			return
		}
		defer it.Close()

		var requestsToDispatch []ReduceableCheckFunc
		for tpl := it.Next(); tpl != nil; tpl = it.Next() {
			tplUserset := tpl.User.GetUserset()
			if onrEqual(tplUserset, req.Goal) {
				resultChan <- CheckResult{true, nil}
				return
			}
			if tplUserset.Relation != Ellipsis {
				// We need to recursively call check here, potentially changing namespaces
				requestsToDispatch = append(requestsToDispatch, cc.dispatch(CheckRequest{
					Start:          tplUserset,
					Goal:           req.Goal,
					AtRevision:     req.AtRevision,
					DepthRemaining: req.DepthRemaining - 1,
				}))
			}
		}
		if it.Err() != nil {
			resultChan <- CheckResult{false, NewCheckFailureErr(it.Err())}
			return
		}
		resultChan <- Any(ctx, requestsToDispatch)
	}
}

func (cc *concurrentChecker) checkUsersetRewrite(ctx context.Context, req CheckRequest, usr *v0.UsersetRewrite) ReduceableCheckFunc {
	switch rw := usr.RewriteOperation.(type) {
	case *v0.UsersetRewrite_Union:
		return cc.checkSetOperation(ctx, req, rw.Union, Any)
	case *v0.UsersetRewrite_Intersection:
		return cc.checkSetOperation(ctx, req, rw.Intersection, All)
	case *v0.UsersetRewrite_Exclusion:
		return cc.checkSetOperation(ctx, req, rw.Exclusion, Difference)
	default:
		return AlwaysFail
	}
}

func (cc *concurrentChecker) checkSetOperation(ctx context.Context, req CheckRequest, so *v0.SetOperation, reducer Reducer) ReduceableCheckFunc {
	var requests []ReduceableCheckFunc
	for _, childOneof := range so.Child {
		switch child := childOneof.ChildType.(type) {
		case *v0.SetOperation_Child_XThis:
			requests = append(requests, cc.checkDirect(ctx, req))
		case *v0.SetOperation_Child_ComputedUserset:
			requests = append(requests, cc.checkComputedUserset(ctx, req, child.ComputedUserset, nil))
		case *v0.SetOperation_Child_UsersetRewrite:
			requests = append(requests, cc.checkUsersetRewrite(ctx, req, child.UsersetRewrite))
		case *v0.SetOperation_Child_TupleToUserset:
			requests = append(requests, cc.checkTupleToUserset(ctx, req, child.TupleToUserset))
		}
	}
	return func(ctx context.Context, resultChan chan<- CheckResult) {
		log.Trace().Object("set operation", req).Stringer("operation", so).Send()
		resultChan <- reducer(ctx, requests)
	}
}

func (cc *concurrentChecker) checkComputedUserset(ctx context.Context, req CheckRequest, cu *v0.ComputedUserset, tpl *v0.RelationTuple) ReduceableCheckFunc {
	var start *v0.ObjectAndRelation
	if cu.Object == v0.ComputedUserset_TUPLE_USERSET_OBJECT {
		if tpl == nil {
			panic("computed userset for tupleset without tuple")
		}

		start = tpl.User.GetUserset()
	} else if cu.Object == v0.ComputedUserset_TUPLE_OBJECT {
		if tpl != nil {
			start = tpl.ObjectAndRelation
		} else {
			start = req.Start
		}
	}

	targetOnr := &v0.ObjectAndRelation{
		Namespace: start.Namespace,
		ObjectId:  start.ObjectId,
		Relation:  cu.Relation,
	}

	// If we will be dispatching to the goal's ONR, then we know that the ONR is a member.
	if onrEqual(req.Goal, targetOnr) {
		return AlwaysMember()
	}

	// Check if the target relation exists. If not, return nothing.
	err := cc.nsm.CheckNamespaceAndRelation(ctx, start.Namespace, cu.Relation, true)
	if err != nil {
		if errors.As(err, &namespace.ErrRelationNotFound{}) {
			return NotMember()
		}

		return CheckError(err)
	}

	return cc.dispatch(CheckRequest{
		Start:          targetOnr,
		Goal:           req.Goal,
		AtRevision:     req.AtRevision,
		DepthRemaining: req.DepthRemaining - 1,
	})
}

func (cc *concurrentChecker) checkTupleToUserset(ctx context.Context, req CheckRequest, ttu *v0.TupleToUserset) ReduceableCheckFunc {
	return func(ctx context.Context, resultChan chan<- CheckResult) {
		log.Trace().Object("ttu", req).Send()
		it, err := cc.ds.QueryTuples(req.Start.Namespace, req.AtRevision).
			WithObjectID(req.Start.ObjectId).
			WithRelation(ttu.Tupleset.Relation).
			Execute(ctx)
		if err != nil {
			resultChan <- CheckResult{false, NewCheckFailureErr(err)}
			return
		}
		defer it.Close()

		var requestsToDispatch []ReduceableCheckFunc
		for tpl := it.Next(); tpl != nil; tpl = it.Next() {
			requestsToDispatch = append(requestsToDispatch, cc.checkComputedUserset(ctx, req, ttu.ComputedUserset, tpl))
		}
		if it.Err() != nil {
			resultChan <- CheckResult{false, NewCheckFailureErr(it.Err())}
			return
		}

		resultChan <- Any(ctx, requestsToDispatch)
	}
}

// All returns whether all of the lazy checks pass, and is used for intersection.
func All(ctx context.Context, requests []ReduceableCheckFunc) CheckResult {
	if len(requests) == 0 {
		return CheckResult{IsMember: false, Err: nil}
	}

	resultChan := make(chan CheckResult, len(requests))
	childCtx, cancelFn := context.WithCancel(ctx)
	defer cancelFn()

	for _, req := range requests {
		go req(childCtx, resultChan)
	}

	for i := 0; i < len(requests); i++ {
		select {
		case result := <-resultChan:
			if result.Err != nil || !result.IsMember {
				return result
			}
		case <-ctx.Done():
			return CheckResult{IsMember: false, Err: NewRequestCanceledErr()}
		}
	}

	return CheckResult{IsMember: true, Err: nil}
}

// CheckError returns the error.
func CheckError(err error) ReduceableCheckFunc {
	return func(ctx context.Context, resultChan chan<- CheckResult) {
		resultChan <- CheckResult{false, err}
	}
}

// AlwaysMember returns that the check always passes.
func AlwaysMember() ReduceableCheckFunc {
	return func(ctx context.Context, resultChan chan<- CheckResult) {
		resultChan <- CheckResult{true, nil}
	}
}

// NotMember returns that the check always returns false.
func NotMember() ReduceableCheckFunc {
	return func(ctx context.Context, resultChan chan<- CheckResult) {
		resultChan <- CheckResult{false, nil}
	}
}

// Any returns whether any one of the lazy checks pass, and is used for union.
func Any(ctx context.Context, requests []ReduceableCheckFunc) CheckResult {
	if len(requests) == 0 {
		return CheckResult{IsMember: false, Err: nil}
	}

	resultChan := make(chan CheckResult, len(requests))
	childCtx, cancelFn := context.WithCancel(ctx)
	defer cancelFn()

	for _, req := range requests {
		go req(childCtx, resultChan)
	}

	var downstreamError error
	for i := 0; i < len(requests); i++ {
		select {
		case result := <-resultChan:
			log.Trace().Object("any result", result).Send()
			if result.Err == nil && result.IsMember {
				return result
			}
			if result.Err != nil {
				downstreamError = result.Err
			}
		case <-ctx.Done():
			log.Trace().Msg("any canceled")
			return CheckResult{IsMember: false, Err: NewRequestCanceledErr()}
		}
	}

	return CheckResult{IsMember: false, Err: downstreamError}
}

// Difference returns whether the first lazy check passes and none of the supsequent checks pass.
func Difference(ctx context.Context, requests []ReduceableCheckFunc) CheckResult {
	childCtx, cancelFn := context.WithCancel(ctx)
	defer cancelFn()

	baseChan := make(chan CheckResult, 1)
	othersChan := make(chan CheckResult, len(requests)-1)

	go requests[0](childCtx, baseChan)
	for _, req := range requests[1:] {
		go req(childCtx, othersChan)
	}

	for i := 0; i < len(requests); i++ {
		select {
		case base := <-baseChan:
			if base.Err != nil || !base.IsMember {
				return base
			}
		case sub := <-othersChan:
			if sub.Err != nil || sub.IsMember {
				return CheckResult{IsMember: false, Err: sub.Err}
			}
		case <-ctx.Done():
			return CheckResult{IsMember: false, Err: NewRequestCanceledErr()}
		}
	}

	return CheckResult{IsMember: true, Err: nil}
}
