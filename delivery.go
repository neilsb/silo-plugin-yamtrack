package main

import (
	pb "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	bolt "go.etcd.io/bbolt"
)

// Preserve v0.1.x event keys, and associate both delivery paths with a history
// ID. Silo's scheduled export also uses the history ID as its event ID, so
// checking that legacy key prevents resending pre-upgrade exports via scrobble.
func deliveryKeys(endpoint string, e *pb.WatchSyncEvent) [][]byte {
	keys := [][]byte{[]byte(digest(endpoint + "\x00" + e.GetEventId()))}
	if id := e.GetWatchHistoryId(); id != "" {
		keys = append(keys,
			[]byte(digest(endpoint+"\x00history\x00"+id)),
			[]byte(digest(endpoint+"\x00"+id)),
		)
	}
	return keys
}

func setDeliveryState(b *bolt.Bucket, keys [][]byte, state string) error {
	for _, key := range keys {
		var err error
		if state == "" {
			err = b.Delete(key)
		} else {
			err = b.Put(key, []byte(state))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func eventFault(id string, f *pb.WatchSyncFault) *pb.WatchSyncApplyResult {
	status := pb.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED
	if f.GetCode() == pb.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY || f.GetCode() == pb.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED {
		status = pb.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY
	}
	return &pb.WatchSyncApplyResult{EventId: id, Status: status, Fault: f}
}
