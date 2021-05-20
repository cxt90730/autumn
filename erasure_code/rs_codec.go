package erasure_code

import (
	"encoding/binary"
	"io"

	"github.com/journeymidnight/autumn/utils"
	"github.com/klauspost/reedsolomon"
)

type ReedSolomon struct{} 

func (ReedSolomon) Reconstruct(input []io.Reader, dataShards int, parityShards int, output []io.Writer) error{
	enc, err := reedsolomon.NewStream(dataShards, parityShards)
	if err != nil {
		return err
	}	
	if err = enc.Reconstruct(input, output) ; err != nil {
			return err
	}
		
	return nil
}

const (
	metaSize = 4
)

func (ReedSolomon) Decode(input [][]byte, dataShards uint32, parityShards uint32, cellSize uint32) ([]byte, error) {

	enc, err := reedsolomon.New(int(dataShards), int(parityShards))
	if err != nil {
		return nil, err
	}

	// Verify the shards
	ok, err := enc.Verify(input)
	if !ok {
		err = enc.Reconstruct(input)
			if err != nil {
				return nil, err
			}
	}

	//join data
	dataLength := binary.BigEndian.Uint32(input[0][:metaSize])
	fullData := make([]byte, dataLength + metaSize)
	actualSize := len(fullData)
	cellNums := utils.Ceil(uint32(actualSize), uint32(cellSize)) / uint32(cellSize)
	for k := uint32(0) ; k <  cellNums ; k ++ {
		i := k / dataShards //row number of input
		j := k % dataShards //column number of input
		n := copy(fullData[(k*cellSize):], input[j][i*cellSize:])
		utils.AssertTrue(n > 0)
	}
	return fullData[metaSize:], nil
}

func (ReedSolomon) Encode(input []byte, dataShards uint32, parityShards uint32, cellSize uint32) ([][]byte, error) {

	enc, err := reedsolomon.New(int(dataShards), int(parityShards))
	if err != nil {
		return nil, err
	}
	rawSize := uint32(len(input))
	actualSize := rawSize + metaSize
	groupSize := int64(dataShards * cellSize)
	//groupSize is not power of 2, can not use utils.Ceil
	clusterSize := (actualSize + uint32(groupSize) - 1) / uint32(groupSize) * uint32(groupSize)
	objectSize := clusterSize / dataShards

	data := make([][]byte, dataShards + parityShards)
	for i := uint32(0) ; i < dataShards + parityShards ; i++ {
		data[i] = make([]byte, objectSize)
	}

	//fill the first cell
	binary.BigEndian.PutUint32(data[0], uint32(rawSize))

	//len of input could be smaller than cellSize - metaSize
	size := utils.Min(len(input), int(cellSize) - metaSize)
	copy(data[0][metaSize:], input[0:size])

	//fill the other cell
	cellNums := utils.Ceil(uint32(actualSize), uint32(cellSize)) / uint32(cellSize)
	for k := uint32(1) ; k <  cellNums ; k ++ {
		i := k / dataShards
		j := k % dataShards
		availData := utils.Min(int(cellSize), int(actualSize- k *cellSize))
		copy(data[j][i*cellSize:], input[k*cellSize-metaSize:k*cellSize-metaSize+uint32(availData)])
	}

	err = enc.Encode(data)
	return data, err
}

