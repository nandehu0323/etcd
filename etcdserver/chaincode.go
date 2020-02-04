// Copyright 2016 The etcd Authors
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

package etcdserver

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"sort"
	"time"

	"github.com/hyperledger/fabric-sdk-go/pkg/common/errors/retry"

	pb "go.etcd.io/etcd/etcdserver/etcdserverpb"
	"go.etcd.io/etcd/lease"
	"go.etcd.io/etcd/mvcc"
	"go.etcd.io/etcd/mvcc/mvccpb"
	"go.etcd.io/etcd/pkg/traceutil"

	"github.com/hyperledger/fabric-sdk-go/pkg/client/channel"
	"github.com/hyperledger/fabric-sdk-go/pkg/core/config"
	"github.com/hyperledger/fabric-sdk-go/pkg/fabsdk"

	"github.com/gogo/protobuf/proto"
	"go.uber.org/zap"
)

const (
	channelID           = "mychannel"
	orgName             = "Org1"
	orgAdmin            = "Admin"
	ordererOrgName      = "OrdererOrg"
	defaultConfFilePath = "/usr/local/configs/config.yaml"
	ccID                = "etcd"
)

// applierCC is the interface for processing chaincode raft messages
type applierCC interface {
	Apply(r *pb.InternalRaftRequest) *applyResult

	Put(txn mvcc.TxnWrite, p *pb.PutRequest) (*pb.PutResponse, *traceutil.Trace, error)
	PutCC(p *pb.PutRequest) (*pb.PutResponse, error)
	Range(ctx context.Context, txn mvcc.TxnRead, r *pb.RangeRequest) (*pb.RangeResponse, error)
	DeleteRange(txn mvcc.TxnWrite, dr *pb.DeleteRangeRequest) (*pb.DeleteRangeResponse, error)
	Txn(rt *pb.TxnRequest) (*pb.TxnResponse, error)
	Compaction(compaction *pb.CompactionRequest) (*pb.CompactionResponse, <-chan struct{}, *traceutil.Trace, error)
}

type applierCCbackend struct {
	s          *EtcdServer
	cc         *channel.Client
	checkPut   checkReqFunc
	checkRange checkReqFunc
}

type chainCodeResponse struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func (s *EtcdServer) newApplierCCbackend() applierCC {
	var confFilePath = flag.String("f", defaultConfFilePath, "path to configuration file")
	flag.Parse()

	base := &applierCCbackend{s: s}
	base.checkPut = func(rv mvcc.ReadView, req *pb.RequestOp) error {
		return base.checkRequestPut(rv, req)
	}
	base.checkRange = func(rv mvcc.ReadView, req *pb.RequestOp) error {
		return base.checkRequestRange(rv, req)
	}
	sdk, err := fabsdk.New(config.FromFile(*confFilePath))
	if err != nil {
		log.Fatal("Failed to create new SDK", err)
	}
	//defer sdk.Close()
	//prepare channel client context using client context
	clientChannelContext := sdk.ChannelContext(channelID, fabsdk.WithUser("User1"), fabsdk.WithOrg(orgName))
	// Channel client is used to query and execute transactions (Org1 is default org)
	client, err := channel.New(clientChannelContext)
	if err != nil {
		log.Fatal("Failed to create new channel client", err)
	}
	base.cc = client
	return base
}

func (a *applierCCbackend) Apply(r *pb.InternalRaftRequest) *applyResult {
	ar := &applyResult{}
	defer func(start time.Time) {
		warnOfExpensiveRequest(a.s.getLogger(), start, &pb.InternalRaftStringer{Request: r}, ar.resp, ar.err)
	}(time.Now())
	// call into a.s.chaincodeBase.F instead of a.F so upper appliers can check individual calls
	switch {
	case r.Range != nil:
		ar.resp, ar.err = a.s.chaincodeBase.Range(context.TODO(), nil, r.Range)
	case r.Put != nil:
		ar.resp, ar.trace, ar.err = a.s.chaincodeBase.Put(nil, r.Put)
	case r.DeleteRange != nil:
		ar.resp, ar.err = a.s.chaincodeBase.DeleteRange(nil, r.DeleteRange)
	case r.Txn != nil:
		ar.resp, ar.err = a.s.chaincodeBase.Txn(r.Txn)
	case r.Compaction != nil:
		ar.resp, ar.physc, ar.trace, ar.err = a.s.chaincodeBase.Compaction(r.Compaction)
	default:
		panic("not implemented")
	}
	return ar
}

func (a *applierCCbackend) Put(txn mvcc.TxnWrite, p *pb.PutRequest) (resp *pb.PutResponse, trace *traceutil.Trace, err error) {
	resp = &pb.PutResponse{}
	resp.Header = &pb.ResponseHeader{}
	trace = traceutil.New("put",
		a.s.getLogger(),
		traceutil.Field{Key: "key", Value: string(p.Key)},
		traceutil.Field{Key: "req_size", Value: proto.Size(p)},
	)
	val, leaseID := p.Value, lease.LeaseID(p.Lease)
	if txn == nil {
		if leaseID != lease.NoLease {
			if l := a.s.lessor.Lookup(leaseID); l == nil {
				return nil, nil, lease.ErrLeaseNotFound
			}
		}
		txn = a.s.KV().Write(trace)
		defer txn.End()
	}

	var rr *mvcc.RangeResult
	if p.IgnoreValue || p.IgnoreLease || p.PrevKv {
		trace.DisableStep()
		rr, err = txn.Range(p.Key, nil, mvcc.RangeOptions{})
		if err != nil {
			return nil, nil, err
		}
		trace.EnableStep()
		trace.Step("get previous kv pair")
	}
	if p.IgnoreValue || p.IgnoreLease {
		if rr == nil || len(rr.KVs) == 0 {
			// ignore_{lease,value} flag expects previous key-value pair
			return nil, nil, ErrKeyNotFound
		}
	}
	if p.IgnoreValue {
		val = rr.KVs[0].Value
	}
	if p.IgnoreLease {
		leaseID = lease.LeaseID(rr.KVs[0].Lease)
	}
	if p.PrevKv {
		if rr != nil && len(rr.KVs) != 0 {
			resp.PrevKv = &rr.KVs[0]
		}
	}

	//fmt.Printf("Key: %s, Val: %s, ID: %s\n", p.Key, val, leaseID)
	resp.Header.Revision = txn.Put(p.Key, val, leaseID)
	trace.AddField(traceutil.Field{Key: "response_revision", Value: resp.Header.Revision})
	return resp, trace, nil
}

func (a *applierCCbackend) PutCC(p *pb.PutRequest) (resp *pb.PutResponse, err error) {
	resp = &pb.PutResponse{}
	resp.Header = &pb.ResponseHeader{}
	trace := traceutil.New("put",
		a.s.getLogger(),
		traceutil.Field{Key: "key", Value: string(p.Key)},
		traceutil.Field{Key: "req_size", Value: proto.Size(p)},
	)
	val, leaseID := p.Value, lease.LeaseID(p.Lease)
	if leaseID != lease.NoLease {
		if l := a.s.lessor.Lookup(leaseID); l == nil {
			return &pb.PutResponse{}, lease.ErrLeaseNotFound
		}
	}
	_, err = a.cc.Execute(channel.Request{ChaincodeID: ccID, Fcn: "Put", Args: [][]byte{p.Key, val}},
		channel.WithRetry(retry.DefaultChannelOpts),
		channel.WithTargetEndpoints("peer0.org1.example.com"),
	)
	if err != nil {
		return &pb.PutResponse{}, err
	}
	trace.AddField(traceutil.Field{Key: "response_revision", Value: resp.Header.Revision})
	return resp, nil
}

func (a *applierCCbackend) DeleteRange(txn mvcc.TxnWrite, dr *pb.DeleteRangeRequest) (*pb.DeleteRangeResponse, error) {
	resp := &pb.DeleteRangeResponse{}
	resp.Header = &pb.ResponseHeader{}
	end := mkGteRange(dr.RangeEnd)

	if txn == nil {
		txn = a.s.kv.Write(traceutil.TODO())
		defer txn.End()
	}

	if dr.PrevKv {
		rr, err := txn.Range(dr.Key, end, mvcc.RangeOptions{})
		if err != nil {
			return nil, err
		}
		if rr != nil {
			resp.PrevKvs = make([]*mvccpb.KeyValue, len(rr.KVs))
			for i := range rr.KVs {
				resp.PrevKvs[i] = &rr.KVs[i]
			}
		}
	}

	resp.Deleted, resp.Header.Revision = txn.DeleteRange(dr.Key, end)
	return resp, nil
}

func (a *applierCCbackend) Range(ctx context.Context, txn mvcc.TxnRead, r *pb.RangeRequest) (*pb.RangeResponse, error) {
	trace := traceutil.Get(ctx)

	resp := &pb.RangeResponse{}
	resp.Header = &pb.ResponseHeader{}

	if txn == nil {
		txn = a.s.kv.Read(trace)
		defer txn.End()
	}

	limit := r.Limit
	if r.SortOrder != pb.RangeRequest_NONE ||
		r.MinModRevision != 0 || r.MaxModRevision != 0 ||
		r.MinCreateRevision != 0 || r.MaxCreateRevision != 0 {
		// fetch everything; sort and truncate afterwards
		limit = 0
	}
	if limit > 0 {
		// fetch one extra for 'more' flag
		limit = limit + 1
	}
	response, err := a.cc.Query(channel.Request{ChaincodeID: ccID, Fcn: "List", Args: [][]byte{mkGteRange(r.Key), mkGteRange(r.RangeEnd)}},
		channel.WithRetry(retry.DefaultChannelOpts),
		channel.WithTargetEndpoints("peer0.org1.example.com"),
	)
	if err != nil {
		log.Fatal("Failed to query funds\n", err)
	}

	var res []chainCodeResponse
	if err := json.Unmarshal(response.Payload, &res); err != nil {
		log.Fatal(err)
	}

	kvs := make([]mvccpb.KeyValue, len(res))
	for i, v := range res {
		kvs[i] = mvccpb.KeyValue{
			Key:   []byte(v.Key),
			Value: []byte(v.Value),
		}
	}

	rr := &mvcc.RangeResult{
		KVs:   kvs,
		Rev:   r.Revision + 1,
		Count: len(kvs),
	}

	if r.MaxModRevision != 0 {
		f := func(kv *mvccpb.KeyValue) bool { return kv.ModRevision > r.MaxModRevision }
		pruneKVs(rr, f)
	}
	if r.MinModRevision != 0 {
		f := func(kv *mvccpb.KeyValue) bool { return kv.ModRevision < r.MinModRevision }
		pruneKVs(rr, f)
	}
	if r.MaxCreateRevision != 0 {
		f := func(kv *mvccpb.KeyValue) bool { return kv.CreateRevision > r.MaxCreateRevision }
		pruneKVs(rr, f)
	}
	if r.MinCreateRevision != 0 {
		f := func(kv *mvccpb.KeyValue) bool { return kv.CreateRevision < r.MinCreateRevision }
		pruneKVs(rr, f)
	}

	sortOrder := r.SortOrder
	if r.SortTarget != pb.RangeRequest_KEY && sortOrder == pb.RangeRequest_NONE {
		// Since current mvcc.Range implementation returns results
		// sorted by keys in lexiographically ascending order,
		// sort ASCEND by default only when target is not 'KEY'
		sortOrder = pb.RangeRequest_ASCEND
	}
	if sortOrder != pb.RangeRequest_NONE {
		var sorter sort.Interface
		switch {
		case r.SortTarget == pb.RangeRequest_KEY:
			sorter = &kvSortByKey{&kvSort{rr.KVs}}
		case r.SortTarget == pb.RangeRequest_VERSION:
			sorter = &kvSortByVersion{&kvSort{rr.KVs}}
		case r.SortTarget == pb.RangeRequest_CREATE:
			sorter = &kvSortByCreate{&kvSort{rr.KVs}}
		case r.SortTarget == pb.RangeRequest_MOD:
			sorter = &kvSortByMod{&kvSort{rr.KVs}}
		case r.SortTarget == pb.RangeRequest_VALUE:
			sorter = &kvSortByValue{&kvSort{rr.KVs}}
		}
		switch {
		case sortOrder == pb.RangeRequest_ASCEND:
			sort.Sort(sorter)
		case sortOrder == pb.RangeRequest_DESCEND:
			sort.Sort(sort.Reverse(sorter))
		}
	}

	if r.Limit > 0 && len(rr.KVs) > int(r.Limit) {
		rr.KVs = rr.KVs[:r.Limit]
		resp.More = true
	}
	trace.Step("filter and sort the key-value pairs")
	resp.Header.Revision = rr.Rev
	resp.Count = int64(rr.Count)
	resp.Kvs = make([]*mvccpb.KeyValue, len(rr.KVs))
	for i := range rr.KVs {
		if r.KeysOnly {
			rr.KVs[i].Value = nil
		}
		resp.Kvs[i] = &rr.KVs[i]
	}
	trace.Step("assemble the response")
	return resp, nil
}

func (a *applierCCbackend) Txn(rt *pb.TxnRequest) (*pb.TxnResponse, error) {
	isWrite := !isTxnReadonly(rt)
	txn := mvcc.NewReadOnlyTxnWrite(a.s.KV().Read(traceutil.TODO()))

	txnPath := compareToPath(txn, rt)
	if isWrite {
		if _, err := checkRequests(txn, rt, txnPath, a.checkPut); err != nil {
			txn.End()
			return nil, err
		}
	}
	if _, err := checkRequests(txn, rt, txnPath, a.checkRange); err != nil {
		txn.End()
		return nil, err
	}

	txnResp, _ := newTxnResp(rt, txnPath)

	// When executing mutable txn ops, etcd must hold the txn lock so
	// readers do not see any intermediate results. Since writes are
	// serialized on the raft loop, the revision in the read view will
	// be the revision of the write txn.
	if isWrite {
		txn.End()
		txn = a.s.KV().Write(traceutil.TODO())
	}
	a.applyTxn(txn, rt, txnPath, txnResp)
	rev := txn.Rev()
	if len(txn.Changes()) != 0 {
		rev++
	}
	txn.End()

	txnResp.Header.Revision = rev
	return txnResp, nil
}

func (a *applierCCbackend) applyTxn(txn mvcc.TxnWrite, rt *pb.TxnRequest, txnPath []bool, tresp *pb.TxnResponse) (txns int) {
	reqs := rt.Success
	if !txnPath[0] {
		reqs = rt.Failure
	}

	lg := a.s.getLogger()
	for i, req := range reqs {
		respi := tresp.Responses[i].Response
		switch tv := req.Request.(type) {
		case *pb.RequestOp_RequestRange:
			resp, err := a.Range(context.TODO(), txn, tv.RequestRange)
			if err != nil {
				if lg != nil {
					lg.Panic("unexpected error during txn", zap.Error(err))
				} else {
					plog.Panicf("unexpected error during txn: %v", err)
				}
			}
			respi.(*pb.ResponseOp_ResponseRange).ResponseRange = resp
		case *pb.RequestOp_RequestPut:
			resp, _, err := a.Put(txn, tv.RequestPut)
			if err != nil {
				if lg != nil {
					lg.Panic("unexpected error during txn", zap.Error(err))
				} else {
					plog.Panicf("unexpected error during txn: %v", err)
				}
			}
			respi.(*pb.ResponseOp_ResponsePut).ResponsePut = resp
		case *pb.RequestOp_RequestDeleteRange:
			resp, err := a.DeleteRange(txn, tv.RequestDeleteRange)
			if err != nil {
				if lg != nil {
					lg.Panic("unexpected error during txn", zap.Error(err))
				} else {
					plog.Panicf("unexpected error during txn: %v", err)
				}
			}
			respi.(*pb.ResponseOp_ResponseDeleteRange).ResponseDeleteRange = resp
		case *pb.RequestOp_RequestTxn:
			resp := respi.(*pb.ResponseOp_ResponseTxn).ResponseTxn
			applyTxns := a.applyTxn(txn, tv.RequestTxn, txnPath[1:], resp)
			txns += applyTxns + 1
			txnPath = txnPath[applyTxns+1:]
		default:
			// empty union
		}
	}
	return txns
}

func (a *applierCCbackend) Compaction(compaction *pb.CompactionRequest) (*pb.CompactionResponse, <-chan struct{}, *traceutil.Trace, error) {
	resp := &pb.CompactionResponse{}
	resp.Header = &pb.ResponseHeader{}
	trace := traceutil.New("compact",
		a.s.getLogger(),
		traceutil.Field{Key: "revision", Value: compaction.Revision},
	)

	ch, err := a.s.KV().Compact(trace, compaction.Revision)
	if err != nil {
		return nil, ch, nil, err
	}
	// get the current revision. which key to get is not important.
	rr, _ := a.s.KV().Range([]byte("compaction"), nil, mvcc.RangeOptions{})
	resp.Header.Revision = rr.Rev
	return resp, ch, trace, err
}

func (a *applierCCbackend) checkRequestPut(rv mvcc.ReadView, reqOp *pb.RequestOp) error {
	tv, ok := reqOp.Request.(*pb.RequestOp_RequestPut)
	if !ok || tv.RequestPut == nil {
		return nil
	}
	req := tv.RequestPut
	if req.IgnoreValue || req.IgnoreLease {
		// expects previous key-value, error if not exist
		rr, err := rv.Range(req.Key, nil, mvcc.RangeOptions{})
		if err != nil {
			return err
		}
		if rr == nil || len(rr.KVs) == 0 {
			return ErrKeyNotFound
		}
	}
	if lease.LeaseID(req.Lease) != lease.NoLease {
		if l := a.s.lessor.Lookup(lease.LeaseID(req.Lease)); l == nil {
			return lease.ErrLeaseNotFound
		}
	}
	return nil
}

func (a *applierCCbackend) checkRequestRange(rv mvcc.ReadView, reqOp *pb.RequestOp) error {
	tv, ok := reqOp.Request.(*pb.RequestOp_RequestRange)
	if !ok || tv.RequestRange == nil {
		return nil
	}
	req := tv.RequestRange
	switch {
	case req.Revision == 0:
		return nil
	case req.Revision > rv.Rev():
		return mvcc.ErrFutureRev
	case req.Revision < rv.FirstRev():
		return mvcc.ErrCompacted
	}
	return nil
}
