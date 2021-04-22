/*
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless  by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package node

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/journeymidnight/autumn/conn"
	"github.com/journeymidnight/autumn/extent"
	"github.com/journeymidnight/autumn/proto/pb"
	"github.com/journeymidnight/autumn/utils"
	"github.com/journeymidnight/autumn/xlog"
	"github.com/pkg/errors"
)

var (
	_ = conn.GetPools
	_ = fmt.Printf
)

//internal services
func (en *ExtentNode) Heartbeat(in *pb.Payload, stream pb.ExtentService_HeartbeatServer) error {
	ticker := time.NewTicker(conn.EchoDuration)
	defer ticker.Stop()

	ctx := stream.Context()
	out := &pb.Payload{Data: []byte("beat")}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := stream.Send(out); err != nil {
				return err
			}
		}
	}
}

func (en *ExtentNode) ReplicateBlocks(ctx context.Context, req *pb.ReplicateBlocksRequest) (*pb.ReplicateBlocksResponse, error) {

	ex := en.getExtent((req.ExtentID))
	if ex == nil {
		return nil, errors.Errorf("no suck extent")
	}
	ex.Lock()
	defer ex.Unlock()
	if ex.CommitLength() != req.Commit {
		return nil, errors.Errorf("primary commitlength is different with replicates %d vs %d", req.Commit, ex.CommitLength())
	}
	ret, end, err := en.AppendWithWal(ex, req.Blocks)
	if err != nil {
		return nil, err
	}
	return &pb.ReplicateBlocksResponse{
		Code:    pb.Code_OK,
		Offsets: ret,
		End : end,
	}, nil

}

func (en *ExtentNode) connPoolOfReplicates(peers []string) ([]*conn.Pool, error) {
	var ret []*conn.Pool
	for _, peer := range peers {
		pool := conn.GetPools().Connect(peer)
		if !pool.IsHealthy() {
			return nil, errors.Errorf("remote peer %s not healthy", peer)
		}
		ret = append(ret, pool)
	}
	return ret, nil
}

func (en *ExtentNode) Append(ctx context.Context, req *pb.AppendRequest) (*pb.AppendResponse, error) {
	
	ex := en.getExtent(req.ExtentID)
	if ex == nil {
		xlog.Logger.Debugf("no extent %d", req.ExtentID)
		return nil, errors.Errorf("not such extent")
	}

	ex.Lock()
	defer ex.Unlock()

	pools, err := en.connPoolOfReplicates(req.Peers)
	if err != nil {
		return nil, err
	}
	offset := ex.CommitLength()

	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	//FIXME: put stopper into sync.Pool
	stopper := utils.NewStopper()

	type Result struct {
		Error   error
		Offsets []uint32
		End uint32
	}
	retChan := make(chan Result, 3)

	//primary
	stopper.RunWorker(func() {
		//ret, err := ex.AppendBlocks(req.Blocks, &offset)
		ret, end, err := en.AppendWithWal(ex, req.Blocks)

		if ret != nil {
			retChan <- Result{Error: err, Offsets: ret, End:end}
		} else {
			retChan <- Result{Error: err}
		}
		xlog.Logger.Debugf("write primary done: %v, %v", ret, err)
	})


	//secondary
	for i := 1; i < 3; i++ {
		j := i
		stopper.RunWorker(func() {
			conn := pools[j].Get()
			client := pb.NewExtentServiceClient(conn)
			res, err := client.ReplicateBlocks(pctx, &pb.ReplicateBlocksRequest{
				ExtentID: req.ExtentID,
				Commit:   offset,
				Blocks:   req.Blocks,
			})
			if res != nil {
				retChan <- Result{Error: err, Offsets: res.Offsets, End: res.End}
			} else {
				retChan <- Result{Error: err}
			}
			xlog.Logger.Debugf("write seconary done %v", err)

		})
	}

	stopper.Wait()
	close(retChan)

	var preOffsets []uint32
	preEnd  := int64(-1)
	for result := range retChan {
		if preOffsets == nil {
			preOffsets = result.Offsets
		}
		if preEnd == -1 {
			preEnd = int64(result.End)
		}
		if result.Error != nil || !utils.EqualUint32(result.Offsets, preOffsets) {
			return nil, result.Error
		}
		if !utils.EqualUint32(result.Offsets, preOffsets) || preEnd != int64(result.End){
			return nil, errors.Errorf("block is not appended at the same offset [%v] vs [%v], end [%v] vs [%v]", 
			result.Offsets, preOffsets, preEnd, result.End)
		}
	}

	return &pb.AppendResponse{
		Code:    pb.Code_OK,
		Offsets: preOffsets,
		End: uint32(preEnd),
	}, nil
}

func errorToCode(err error) pb.Code {
	switch err {
	case extent.EndOfStream:
		return pb.Code_EndOfStream
	case extent.EndOfExtent:
		return pb.Code_EndOfExtent
	case nil:
		return pb.Code_OK
	default:
		xlog.Logger.Fatalf("unknown err met %v", err)
		return pb.Code_ERROR
	}
}
func (en *ExtentNode) ReadBlocks(ctx context.Context, req *pb.ReadBlocksRequest) (*pb.ReadBlocksResponse, error) {
	ex := en.getExtent(req.ExtentID)
	if ex == nil {
		return nil, errors.Errorf("no such extent")
	}
	blocks, _, end, err := ex.ReadBlocks(req.Offset, req.NumOfBlocks, (32 << 20))
	if err != nil && err != extent.EndOfStream && err != extent.EndOfExtent {
		xlog.Logger.Infof("request extentID: %d, offset: %d, numOfBlocks: %d: %v", req.ExtentID, req.Offset, req.NumOfBlocks, err)
		return nil, err
	}

	xlog.Logger.Debugf("request extentID: %d, offset: %d, numOfBlocks: %d, response len(%d), %v ", req.ExtentID, req.Offset, req.NumOfBlocks,
		len(blocks), err)

	return &pb.ReadBlocksResponse{
		Code:   errorToCode(err),
		Blocks: blocks,
		End: end,
	}, nil
}

func (en *ExtentNode) AllocExtent(ctx context.Context, req *pb.AllocExtentRequest) (*pb.AllocExtentResponse, error) {
	i := rand.Intn(len(en.diskFSs))
	ex, err := en.diskFSs[i].AllocExtent(req.ExtentID)
	if err != nil {
		xlog.Logger.Warnf("can not alloc extent %d, [%s]", req.ExtentID, err.Error())
		return nil, err
	}
	en.extentMap.Store(req.ExtentID, ex)

	return &pb.AllocExtentResponse{
		Code: pb.Code_OK,
	}, nil
}

func (en *ExtentNode) Seal(ctx context.Context, req *pb.SealRequest) (*pb.SealResponse, error) {
	ex := en.getExtent(req.ExtentID)
	if ex != nil {
		return nil, errors.Errorf("have extent, can not alloc new")
	}
	err := ex.Seal(req.CommitLength)
	if err != nil {
		xlog.Logger.Warnf(err.Error())
		return nil, err
	}
	return &pb.SealResponse{Code: pb.Code_OK}, nil

}
func (en *ExtentNode) CommitLength(ctx context.Context, req *pb.CommitLengthRequest) (*pb.CommitLengthResponse, error) {
	ex := en.getExtent(req.ExtentID)
	if ex != nil {
		return nil, errors.Errorf("have extent, can not alloc new")
	}

	l := ex.CommitLength()
	return &pb.CommitLengthResponse{
		Code:   pb.Code_OK,
		Length: l,
	}, nil

}

func (en *ExtentNode) ReadEntries(ctx context.Context, req *pb.ReadEntriesRequest) (*pb.ReadEntriesResponse, error) {
	ex := en.getExtent(req.ExtentID)
	if ex == nil {
		return nil, errors.Errorf("no such extent")
	}
	replay := false
	if req.Replay > 0 {
		replay = true
	}
	ei, end, err := ex.ReadEntries(req.Offset, (25 << 20), replay)
	if err != nil && err != extent.EndOfStream && err != extent.EndOfExtent {
		xlog.Logger.Infof("request ReadEntires extentID: %d, offset: %d, : %v", req.ExtentID, req.Offset, err)
		return nil, err
	}

	return &pb.ReadEntriesResponse{
		Code:      errorToCode(err),
		Entries:   ei,
		End: end,
	}, nil
}
