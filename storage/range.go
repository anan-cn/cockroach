// Copyright 2014 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied.  See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Spencer Kimball (spencer.kimball@gmail.com)

package storage

import (
	"bytes"
	"container/list"
	"encoding/gob"
	"reflect"
	"sync"
	"time"

	"github.com/cockroachdb/cockroach/gossip"
	"github.com/cockroachdb/cockroach/util"
	"github.com/golang/glog"
	yaml "gopkg.in/yaml.v1"
)

// configPrefixes describes administrative configuration maps
// affecting ranges of the key-value map by key prefix.
var configPrefixes = []struct {
	keyPrefix Key         // Range key prefix
	gossipKey string      // Gossip key
	configI   interface{} // Config struct interface
	dirty     bool        // Info in this config has changed; need to re-init and gossip
}{
	{KeyConfigAccountingPrefix, gossip.KeyConfigAccounting, AcctConfig{}, true},
	{KeyConfigPermissionPrefix, gossip.KeyConfigPermission, PermConfig{}, true},
	{KeyConfigZonePrefix, gossip.KeyConfigZone, ZoneConfig{}, true},
}

// A RangeMetadata holds information about the range, including
// range ID and start and end keys, and replicas slice.
type RangeMetadata struct {
	RangeID  int64
	StartKey Key
	EndKey   Key
	Replicas RangeLocations
}

// A Range is a contiguous keyspace with writes managed via an
// instance of the Raft consensus algorithm. Many ranges may exist
// in a store and they are unlikely to be contiguous. Ranges are
// independent units and are responsible for maintaining their own
// integrity by replacing failed replicas, splitting and merging
// as appropriate.
type Range struct {
	Meta      RangeMetadata
	engine    Engine         // The underlying key-value store
	allocator *allocator     // Makes allocation decisions
	gossip    *gossip.Gossip // Range may gossip based on contents
	mu        sync.Mutex     // Protects the pending list
	pending   *list.List     // Not-yet-proposed log entries
	// TODO(andybons): raft instance goes here.
}

// NewRange initializes the range starting at key.
func NewRange(meta RangeMetadata, engine Engine, allocator *allocator, gossip *gossip.Gossip) *Range {
	r := &Range{
		Meta:      meta,
		engine:    engine,
		allocator: allocator,
		gossip:    gossip,
		pending:   list.New(),
	}
	r.maybeGossipFirstRange()
	r.maybeGossipConfigMaps()
	return r
}

// ReadOnlyCmd executes a read-only command against the store. If this
// server has executed a raft command or heartbeat at a timestamp
// greater than the read timestamp, we can satisfy the read locally
// without further ado. Otherwise, we must contact ceil(N/2) raft
// participants with a ReadQuorum RPC to determine with certainty
// whether our local data is up to date. The requests to other
// participants simply check their in-memory latest-write-cache to
// determine whether the key in question was updated since this node's
// last raft heartbeat, but before this command's read timestamp. In
// the process, the latest-read-cache timestamp for the key being read
// is updated on each participant. If this replica has stale info for
// the key, an error is returned to the client to retry at the replica
// with newer information.
func (r *Range) ReadOnlyCmd(method string, args, reply interface{}) error {
	if r == nil {
		return util.Errorf("invalid node specification")
	}
	return nil
}

// ReadWriteCmd executes a read-write command against the store. If
// this node is the raft leader, it proposes the write to the other
// raft participants. Otherwise, the write is forwarded via a
// FollowerPropose RPC to the leader and this replica waits for an ACK
// to execute the command locally and return the result to the
// requesting client.
//
// Commands which mutate the store must be proposed as part of the
// raft consensus write protocol. Only after committed can the command
// be executed. To facilitate this, ReadWriteCmd returns a channel
// which is signaled upon completion.
func (r *Range) ReadWriteCmd(method string, args, reply interface{}) <-chan error {
	if r == nil {
		c := make(chan error, 1)
		c <- util.Errorf("invalid node specification")
		return c
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	logEntry := &LogEntry{
		Method: method,
		Args:   args,
		Reply:  reply,
		done:   make(chan error, 1),
	}
	r.pending.PushBack(logEntry)

	return logEntry.done
}

// maybeGossipFirstRange gossips the range locations if this range is
// the start of the key space.
func (r *Range) maybeGossipFirstRange() {
	// TODO(spencer): only do this if we're the leader of the consensus group.
	// if r.isLeader() {
	if r.gossip != nil && bytes.Equal(r.Meta.StartKey, KeyMin) {
		if err := r.gossip.AddInfo(gossip.KeyFirstRangeMetadata, r.Meta.Replicas, 0*time.Second); err != nil {
			glog.Errorf("failed to gossip first range metadata: %v", err)
		}
	}
}

// maybeGossipConfigMaps gossips configuration maps if their data
// falls within the range and their contents are marked
// dirty. Configuration maps include accounting, permissions, and
// zones.
func (r *Range) maybeGossipConfigMaps() {
	// TODO(spencer): only do this if we're the leader of the consensus group.
	// if r.isLeader() {
	if r.gossip != nil {
		for _, cp := range configPrefixes {
			if cp.dirty && r.containsKey(cp.keyPrefix) {
				confMap, err := r.initConfigMap(cp.keyPrefix, cp.configI)
				if err != nil {
					glog.Errorf("failed init %s configuration map: %v", cp.gossipKey, err)
					continue
				} else {
					if err := r.gossip.AddInfo(cp.gossipKey, confMap, 0*time.Second); err != nil {
						glog.Errorf("failed to gossip %s configuration map: %v", cp.gossipKey, err)
						continue
					}
				}
				cp.dirty = false
			}
		}
	}
}

// initConfigMap initializes a prefix config map for the specified key
// prefix. Prefix configuration maps include accounting, permissions,
// and zones.
func (r *Range) initConfigMap(keyPrefix Key, configI interface{}) (*prefixConfigMap, error) {
	// TODO(spencer): need to make sure range splitting never
	// crosses a configuration map's key prefix.
	kvs, err := r.engine.scan(keyPrefix, PrefixEndKey(keyPrefix), 0)
	if err != nil {
		return nil, err
	}
	var configs []*prefixConfig
	for _, kv := range kvs {
		// Instantiate an instance of the config type by unmarshalling
		// YAML text from the Value into a new instance of configI.
		config := reflect.New(reflect.TypeOf(configI)).Interface()
		if err := yaml.Unmarshal(kv.Value.Bytes, config); err != nil {
			return nil, util.Errorf("unable to unmarshal yaml: %s: %v", string(kv.Value.Bytes), err)
		}
		configs = append(configs, &prefixConfig{prefix: bytes.TrimPrefix(kv.Key, keyPrefix), config: config})
	}
	return newPrefixConfigMap(configs)
}

// containsKey returns whether this range contains the specified key.
func (r *Range) containsKey(key Key) bool {
	return bytes.Compare(r.Meta.StartKey, key) <= 0 &&
		bytes.Compare(r.Meta.EndKey, key) > 0
}

// executeCmd switches over the method and multiplexes to execute the
// appropriate storage API command.
func (r *Range) executeCmd(method string, args, reply interface{}) error {
	switch method {
	case "Contains":
		r.Contains(args.(*ContainsRequest), reply.(*ContainsResponse))
	case "Get":
		r.Get(args.(*GetRequest), reply.(*GetResponse))
	case "Put":
		r.Put(args.(*PutRequest), reply.(*PutResponse))
	case "Increment":
		r.Increment(args.(*IncrementRequest), reply.(*IncrementResponse))
	case "Delete":
		r.Delete(args.(*DeleteRequest), reply.(*DeleteResponse))
	case "DeleteRange":
		r.DeleteRange(args.(*DeleteRangeRequest), reply.(*DeleteRangeResponse))
	case "Scan":
		r.Scan(args.(*ScanRequest), reply.(*ScanResponse))
	case "EndTransaction":
		r.EndTransaction(args.(*EndTransactionRequest), reply.(*EndTransactionResponse))
	case "AccumulateTS":
		r.AccumulateTS(args.(*AccumulateTSRequest), reply.(*AccumulateTSResponse))
	case "ReapQueue":
		r.ReapQueue(args.(*ReapQueueRequest), reply.(*ReapQueueResponse))
	case "EnqueueUpdate":
		r.EnqueueUpdate(args.(*EnqueueUpdateRequest), reply.(*EnqueueUpdateResponse))
	case "EnqueueMessage":
		r.EnqueueMessage(args.(*EnqueueMessageRequest), reply.(*EnqueueMessageResponse))
	case "InternalRangeLookup":
		r.InternalRangeLookup(args.(*InternalRangeLookupRequest), reply.(*InternalRangeLookupResponse))
	default:
		return util.Errorf("unrecognized command type: %s", method)
	}
	return nil
}

// Contains verifies the existence of a key in the key value store.
func (r *Range) Contains(args *ContainsRequest, reply *ContainsResponse) {
	val, err := r.engine.get(args.Key)
	if err != nil {
		reply.Error = err
		return
	}
	if val.Bytes != nil {
		reply.Exists = true
	}
}

// Get returns the value for a specified key.
func (r *Range) Get(args *GetRequest, reply *GetResponse) {
	reply.Value, reply.Error = r.engine.get(args.Key)
}

// Put sets the value for a specified key. Conditional puts are supported.
func (r *Range) Put(args *PutRequest, reply *PutResponse) {
	// Handle conditional put.
	if args.ExpValue != nil {
		// Handle check for non-existence of key.
		val, err := r.engine.get(args.Key)
		if err != nil {
			reply.Error = err
			return
		}
		if args.ExpValue.Bytes == nil && val.Bytes != nil {
			reply.Error = util.Errorf("key %q already exists", args.Key)
			return
		} else if args.ExpValue != nil {
			// Handle check for existence when there is no key.
			if val.Bytes == nil {
				reply.Error = util.Errorf("key %q does not exist", args.Key)
				return
			} else if !bytes.Equal(args.ExpValue.Bytes, val.Bytes) {
				reply.ActualValue.Bytes = val.Bytes
				reply.Error = util.Errorf("key %q does not match existing", args.Key)
				return
			}
		}
	}
	if err := r.engine.put(args.Key, args.Value); err != nil {
		reply.Error = err
		return
	}
	// Check whether this put has modified a configuration map.
	for _, cp := range configPrefixes {
		if bytes.HasPrefix(args.Key, cp.keyPrefix) {
			cp.dirty = true
			r.maybeGossipConfigMaps()
			break
		}
	}
}

// Increment increments the value (interpreted as varint64 encoded) and
// returns the newly incremented value (encoded as varint64). If no
// value exists for the key, zero is incremented.
func (r *Range) Increment(args *IncrementRequest, reply *IncrementResponse) {
	reply.NewValue, reply.Error = increment(r.engine, args.Key, args.Increment, args.Timestamp)
}

// Delete deletes the key and value specified by key.
func (r *Range) Delete(args *DeleteRequest, reply *DeleteResponse) {
	if err := r.engine.del(args.Key); err != nil {
		reply.Error = err
	}
}

// DeleteRange deletes the range of key/value pairs specified by
// start and end keys.
func (r *Range) DeleteRange(args *DeleteRangeRequest, reply *DeleteRangeResponse) {
	reply.Error = util.Error("unimplemented")
}

// Scan scans the key range specified by start key through end key up
// to some maximum number of results. The last key of the iteration is
// returned with the reply.
func (r *Range) Scan(args *ScanRequest, reply *ScanResponse) {
	reply.Rows, reply.Error = r.engine.scan(args.StartKey, args.EndKey, args.MaxResults)
}

// EndTransaction either commits or aborts (rolls back) an extant
// transaction according to the args.Commit parameter.
func (r *Range) EndTransaction(args *EndTransactionRequest, reply *EndTransactionResponse) {
	reply.Error = util.Error("unimplemented")
}

// AccumulateTS is used internally to aggregate statistics over key
// ranges throughout the distributed cluster.
func (r *Range) AccumulateTS(args *AccumulateTSRequest, reply *AccumulateTSResponse) {
	reply.Error = util.Error("unimplemented")
}

// ReapQueue destructively queries messages from a delivery inbox
// queue. This method must be called from within a transaction.
func (r *Range) ReapQueue(args *ReapQueueRequest, reply *ReapQueueResponse) {
	reply.Error = util.Error("unimplemented")
}

// EnqueueUpdate sidelines an update for asynchronous execution.
// AccumulateTS updates are sent this way. Eventually-consistent indexes
// are also built using update queues. Crucially, the enqueue happens
// as part of the caller's transaction, so is guaranteed to be
// executed if the transaction succeeded.
func (r *Range) EnqueueUpdate(args *EnqueueUpdateRequest, reply *EnqueueUpdateResponse) {
	reply.Error = util.Error("unimplemented")
}

// EnqueueMessage enqueues a message (Value) for delivery to a
// recipient inbox.
func (r *Range) EnqueueMessage(args *EnqueueMessageRequest, reply *EnqueueMessageResponse) {
	reply.Error = util.Error("unimplemented")
}

// InternalRangeLookup looks up the metadata info for the given args.Key.
// args.Key should be a metadata key, which are of the form "\0\0meta[12]<encoded_key>".
func (r *Range) InternalRangeLookup(args *InternalRangeLookupRequest, reply *InternalRangeLookupResponse) {
	if !bytes.HasPrefix(args.Key, KeyMetaPrefix) {
		reply.Error = util.Errorf("Invalid metadata key: %q", args.Key)
		return
	}

	// Validate that key is not outside the range. Since the keys encoded in metadata keys are
	// the end keys of the range the metadata represent, the check args.Key >= r.Meta.StartKey
	// may result in false negatives.
	if bytes.Compare(args.Key, r.Meta.EndKey) >= 0 {
		reply.Error = util.Errorf("Key outside the range %v with end key %q", r.Meta.RangeID, r.Meta.EndKey)
		return
	}

	// We want to search for the metadata key just greater than args.Key.
	nextKey := MakeKey(args.Key, Key{0})
	kvs, err := r.engine.scan(nextKey, nil, 1)
	if err != nil {
		reply.Error = err
		return
	}
	// We should have gotten the key with the same metadata level prefix as we queried.
	metaPrefix := args.Key[0:len(KeyMeta1Prefix)]
	if len(kvs) != 1 || !bytes.HasPrefix(kvs[0].Key, metaPrefix) {
		reply.Error = util.Errorf("Key not found in range %v", r.Meta.RangeID)
		return
	}

	if err = gob.NewDecoder(bytes.NewBuffer(kvs[0].Value.Bytes)).Decode(reply.Locations); err != nil {
		reply.Error = err
		return
	}
	if bytes.Compare(args.Key, reply.Locations.StartKey) < 0 {
		// args.Key doesn't belong to this range. We are perhaps searching the wrong node?
		reply.Error = util.Errorf("No range found for key %q in range: %+v", args.Key, r.Meta)
		return
	}
	reply.EndKey = kvs[0].Key
}
