package stream_manager

import (
	"sort"

	"github.com/journeymidnight/autumn/xlog"
	"github.com/pkg/errors"
)

type AllocExtentPolicy interface {
	AllocExtent([]*NodeStatus, int, []uint64) ([]*NodeStatus, error)
}

type SimplePolicy struct{}

func (sp *SimplePolicy) AllocExtent(ns []*NodeStatus, count int, keepNodes []uint64) ([]*NodeStatus, error) {

	xlog.Logger.Debugf("alloc extents %d from %d", count, len(ns))
	sort.Slice(ns, func(a, b int) bool {
		if ns[a].LastEcho().After(ns[b].LastEcho()) {
			return true
		} else if ns[a].LastEcho().Before(ns[b].LastEcho()) {
			return false
		}
		return ns[a].free >  ns[b].free
	})

	set := make(map[uint64]bool)
	for _, id := range keepNodes {
		set[id] = true
	}
	if len(ns) < count {
		return nil, errors.New("not enough nodes")
	}

	var ret []*NodeStatus
	for i := 0; i < count; i++ {
		if _, ok := set[ns[i].NodeID]; !ok {
			ret = append(ret, ns[i])
		}
	}
	if len(ret) < count {
		return nil, errors.Errorf("cannot find enough nodes")
	}
	return ret, nil
}
