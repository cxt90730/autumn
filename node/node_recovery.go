package node

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"os"
	"path"
	"sync/atomic"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/journeymidnight/autumn/conn"
	"github.com/journeymidnight/autumn/erasure_code"
	"github.com/journeymidnight/autumn/extent"
	"github.com/journeymidnight/autumn/manager/stream_manager"
	"github.com/journeymidnight/autumn/proto/pb"
	"github.com/journeymidnight/autumn/utils"
	"github.com/journeymidnight/autumn/wire_errors"
	"github.com/journeymidnight/autumn/xlog"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
)

func (en *ExtentNode) copyRemoteExtent(conn *grpc.ClientConn, extentID uint64, targetFile *os.File) error{
	c := pb.NewExtentServiceClient(conn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	copyStream, err := c.CopyExtent(ctx, &pb.CopyExtentRequest{
		ExtentID: extentID,
	})
	//copyStream will close when met non-nil error

	if err != nil {
		return err
	}

	var header *pb.CopyResponseHeader
	//
	res, err := copyStream.Recv()
	if err != nil {
		return err
	}
	if res.GetHeader() == nil {
		return errors.New("copyRemoteExtent: GetHeader is nil")
	}

	header = proto.Clone(res.GetHeader()).(*pb.CopyResponseHeader)

	n := 0
	for {
		res, err := copyStream.Recv()
		if err != nil && err != io.EOF {
			return err
		}
		payload := res.GetPayload()
		if len(payload) > 0 {
			if _, err = targetFile.Write(payload); err != nil {
				return err
			}
			n += len(payload)
		} else {
			utils.AssertTrue(err == io.EOF)
			break
		}
		xlog.Logger.Debugf("recevied data %d", len(payload))
	}

	//check header len
	if n != int(header.PayloadLen) {
		return errors.Errorf("header is %v, got data length %d", header,n)
	}

	//rewind to start for reading
	targetFile.Seek(0, os.SEEK_SET)
	return nil
}

func (en *ExtentNode) recoveryReplicateExtent(extentInfo *pb.ExtentInfo, task *pb.RecoveryTask, targetFile *os.File) error {
	conn := en.chooseAliveNode(extentInfo, task.ReplaceID)
	if conn == nil {
		xlog.Logger.Warnf("runRecoveryTask: can not find remote connect")
		return errors.Errorf("runRecoveryTask: can not find remote connect")
	}

	if err := en.copyRemoteExtent(conn, extentInfo.ExtentID, targetFile) ; err != nil {
		xlog.Logger.Warnf("recoveryReplicateExtent: [%s]", err.Error())
		return  err
	}
    return nil
}

func (en *ExtentNode) recoveryErasureExtent(extentInfo *pb.ExtentInfo, task *pb.RecoveryTask, targetFile *os.File) error {

	conns, replacingIndex := en.chooseECAliveNode(extentInfo, task.ReplaceID)
	if conns == nil {
		return errors.Errorf("ErasureExtent: can not find remote nodes")
	}
	if replacingIndex == -1 {
		return errors.Errorf("task.ReplaceID is %d, not find int extentInfo", task.ReplaceID)
	}

	fmt.Printf("replacingIndex is %d\n", replacingIndex)
	//FIXME: do not use tmp dir
	tmpDir, err := ioutil.TempDir(os.TempDir(), "recoveryEC")
	if err != nil {
		xlog.Logger.Warnf("ErasureExtent: can not create tmpDir")
		os.RemoveAll(tmpDir)
	}
	defer os.RemoveAll(tmpDir)

	tmpFiles := make([]*os.File, len(conns))
	for i := range conns {
		if conns[i] != nil {
			f, err := os.Create(fmt.Sprintf("%s/%d", tmpDir, i))
			if err != nil {
				return err
			}
			tmpFiles[i] = f
		}
	}

	stopper := utils.NewStopper()
	var completes int32
	for i := range conns {
		j := i
		stopper.RunWorker(func() {
			if conns[j] == nil {
				return
			}
			if err = en.copyRemoteExtent(conns[j], extentInfo.ExtentID, tmpFiles[j]); err != nil {
				xlog.Logger.Warnf("ErasureExtent can not copyRemoteExtent %v", err)
				return
			}
			atomic.AddInt32(&completes,1)
		})
	}
	stopper.Wait()

	if completes != int32(len(extentInfo.Replicates)) {
		return errors.Errorf("can not call enought shards")
	}

	//preapre input and output
	input := make([]io.Reader, len(conns))
	for i := range input {
		if tmpFiles[i] == nil {
			input[i] = nil
		} else {
			input[i] = tmpFiles[i]
		}
	}

	output := make([]io.Writer, len(conns))
	output[replacingIndex] = targetFile
	
	//successfull to get data, reconstruct
	fmt.Printf("input is %+v", input)
	fmt.Printf("output is %+v", output)

	err = erasure_code.ReedSolomon{}.Reconstruct(input, len(extentInfo.Replicates), len(extentInfo.Parity),  output)
	if err != nil {
		return err
	}

	return nil
}

//choose at least n alive node from extentInfo
//replics: return one connection
//EC: return (datashards+parity) connections
func (en *ExtentNode) chooseAliveNode(extentInfo *pb.ExtentInfo, except uint64) *grpc.ClientConn {
	addrs := en.em.GetPeers(extentInfo.ExtentID)
	if addrs == nil {
		return nil
	}
	utils.AssertTrue(len(addrs) == len(extentInfo.Replicates))
	for i := range extentInfo.Replicates {
		if extentInfo.Replicates[i] == except {
			continue
		}
		pool , err := conn.GetPools().Get(addrs[i])
		if err != nil {
			continue
		}
		return pool.Get()
	}

	return nil
}

func (en *ExtentNode) chooseECAliveNode(extentInfo *pb.ExtentInfo, except uint64) ([]*grpc.ClientConn, int){
	var missingIndex = -1

	addrs := en.em.GetPeers(extentInfo.ExtentID)
	if addrs == nil {
		return nil, missingIndex
	}
	utils.AssertTrue(len(addrs) == len(extentInfo.Replicates) + len(extentInfo.Parity))
	activeConns := 0
	conns := make([]*grpc.ClientConn, len(addrs))

	nodes := make([]uint64, len(addrs))
	copy(nodes, extentInfo.Replicates)
	copy(nodes[len(extentInfo.Replicates):], extentInfo.Parity)
	for i := 0;i  < len(nodes);i ++ {
		//skip
		if nodes[i] == except {
			missingIndex = i
			continue
		}
		pool , err := conn.GetPools().Get(addrs[i])
		if err != nil {
			conns[i] = nil
		} else {
			conns[i] = pool.Get()
			activeConns++
		}
	}

	if activeConns == len(extentInfo.Replicates) {
		return conns, missingIndex
	}

	return nil, missingIndex
}


func (en *ExtentNode) runRecoveryTask(task *pb.RecoveryTask,extentInfo *pb.ExtentInfo, targetFile *os.File, targetFilePath string) {
	
		atomic.AddInt32(&en.recoveryTaskNum, 1)
		defer func(){
			atomic.AddInt32(&en.recoveryTaskNum, ^int32(0))
		}()

		isEC := len(extentInfo.Parity) > 0
		var err error
		//loop 
		for {
			if stream_manager.FindReplaceSlot(extentInfo, task.ReplaceID) == -1  {
				targetFile.Close()
				os.Remove(targetFilePath)
				return
			}
			if isEC == false {
				err = en.recoveryReplicateExtent(extentInfo, task, targetFile)
			} else {
				err = en.recoveryErasureExtent(extentInfo, task, targetFile)
			}

			if err == nil {
				break
			}
			xlog.Logger.Warnf(err.Error())
			time.Sleep(30*time.Second)
			extentInfo = en.em.Update(task.ExtentID) //get the latest extentInfo
		}
		//rename file from XX.XX.copy to XX.ext
		extentFileName := fmt.Sprintf("%s.ext", path.Dir(targetFilePath))
		utils.Check(os.Rename(targetFilePath, extentFileName))
		//add targetFilePath to extent
		utils.Check(targetFile.Close())
		ex, err := extent.OpenExtent(targetFilePath)
		utils.Check(err)
		en.setExtent(ex.ID, ex)
}


func (en *ExtentNode) CopyExtent(req *pb.CopyExtentRequest, stream pb.ExtentService_CopyExtentServer) error {
	errDone := func(err error, stream pb.ExtentService_CopyExtentServer) (error) {
		code, desCode := wire_errors.ConvertToPBCode(err)
		ret := pb.CopyExtentResponse{
			Data:&pb.CopyExtentResponse_Header{
				Header: &pb.CopyResponseHeader{
					Code: code,
					CodeDes: desCode,
				},
			},
		}
		stream.Send(&ret)
		return io.EOF
	}

	extent := en.getExtent(req.ExtentID)
	if extent == nil {
		return errDone(errors.New("no such extentID"), stream)
	}

	extentInfo := en.em.Update(req.ExtentID)
	if extentInfo == nil {
		return errDone(errors.New("no such extentInfo"), stream)
	}

	if !extent.IsSeal() {
		if extent.CommitLength() >= uint32(extentInfo.SealedLength) {
			extent.Seal(uint32(extentInfo.SealedLength))
		} else {
			//find local node's data should be recoveried
			ctx, cancel := context.WithTimeout(context.Background(), 5 * time.Second)
			en.smClient.SubmitRecoveryTask(ctx, extent.ID, en.nodeID)
			cancel()
			return errDone(errors.Errorf("extent %d on node %d is not complete", req.ExtentID, en.nodeID), stream)
		}
	} else if extent.CommitLength() != uint32(extentInfo.SealedLength){
		//extent.CommitLength() < extentInfo.SealedLength
		//node_service.go would NEVER seal this extent
		//check code "func (ex *Extent) Seal"
		return errDone(errors.New("should never happen"), stream)
	}


	stream.Send(&pb.CopyExtentResponse{
		Data:&pb.CopyExtentResponse_Header{
			Header: &pb.CopyResponseHeader{
				Code: pb.Code_OK,
				PayloadLen: uint64(extent.CommitLength()),
			},
		},
	})

	fmt.Printf("extent size is %d\n\n", extent.CommitLength())

	reader := extent.GetReader()
	buf := make([]byte, 512 << 10)
	for {
		n, err := reader.Read(buf)
		if err != nil && err != io.EOF {
			return err
		}
		if n == 0 {
			break
		}
		if err = stream.Send(&pb.CopyExtentResponse{
			Data:&pb.CopyExtentResponse_Payload{
				Payload: buf[:n],
			},
		}); err != nil {
			return err
		}
		
		time.Sleep(1 * time.Millisecond)
	}
	return nil
}



func (en *ExtentNode) RequireRecovery(ctx context.Context, req *pb.RequireRecoveryRequest) (*pb.RequireRecoveryResponse, error) {

	errDone := func(err error) (*pb.RequireRecoveryResponse, error) {
		code, desCode := wire_errors.ConvertToPBCode(err)
		return &pb.RequireRecoveryResponse{
			Code:    code,
			CodeDes: desCode,
		}, nil
	}

	n := atomic.LoadInt32(&en.recoveryTaskNum)
	if n < MaxConcurrentTask {
		//reply accept

		//create
		xlog.Logger.Infof("run recovery task %+v on node %d\n", req.Task, en.nodeID)

		extentInfo := en.em.Update(req.Task.ExtentID) //get the latest extentInfo

		if stream_manager.FindReplaceSlot(extentInfo, req.Task.ReplaceID) == -1  {
			xlog.Logger.Infof("task %v is not valid to info %v", req.Task, extentInfo)
		}

		if extentInfo.SealedLength == 0 {
			return errDone(errors.New("extent should be sealed"))
		}

		//create targetFile
		//choose one disk
		i := rand.Intn(len(en.diskFSs))
		targetFile, targetFilePath, err := en.diskFSs[i].AllocCopyExtent(extentInfo.ExtentID, req.Task.ReplaceID)
		if err != nil {
			xlog.Logger.Warnf("can not create CopyExtent copy target [%s]", err.Error())
			return errDone(err)
		}
		go en.runRecoveryTask(req.Task, extentInfo, targetFile, targetFilePath)

		return &pb.RequireRecoveryResponse{
			Code: pb.Code_OK,
		}, nil
	}

	//reply will not accept
	return errDone(errors.New("exceed MaxConcurrentTask, please wait..."))
}