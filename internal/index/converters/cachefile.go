package converters

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync"
	"unsafe"

	"github.com/spq/pkappa2/internal/index"
)

type (
	cacheFile struct {
		file      *os.File
		cachePath string
		rwmutex   sync.RWMutex
		fileSize  int64
		freeSize  int64
		freeStart int64

		streamInfos map[uint64]streamInfo
	}

	streamInfo struct {
		offset int64
		size   uint64
	}

	// File format:
	// [u64 stream id] [u8 varint chunk sizes] [client data] [server data]
	converterStreamSection struct {
		StreamID uint64
	}
)

const (
	headerSize = int64(unsafe.Sizeof(converterStreamSection{}))

	// cleanup if at least 16 MiB are free and at least 50%
	cleanupMinFreeSize   = 16 * 1024 * 1024
	cleanupMinFreeFactor = 0.5
)

func readVarInt(r io.ByteReader) (uint64, int, error) {
	bytes := 0
	result := uint64(0)
	for {
		b, err := r.ReadByte()
		if err != nil {
			return 0, 0, err
		}
		bytes++
		result <<= 7
		result |= uint64(b & 0x7f)
		if b < 0x80 {
			break
		}
	}
	return result, bytes, nil
}

func NewCacheFile(cachePath string) (*cacheFile, error) {
	file, err := os.OpenFile(cachePath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}

	res := cacheFile{
		file:        file,
		cachePath:   cachePath,
		streamInfos: map[uint64]streamInfo{},
	}

	// Read all stream ids
	for buffer := bufio.NewReader(file); ; {
		streamSection := converterStreamSection{}
		if err := binary.Read(buffer, binary.LittleEndian, &streamSection); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("failed to read stream header: %w", err)
		}
		res.fileSize += headerSize

		// Read total data size of the stream by adding all chunk sizes up.
		lengthSize, dataSize := uint64(0), uint64(0)
		for nZeros := 0; nZeros < 2; {
			sz, n, err := readVarInt(buffer)
			if err != nil {
				return nil, fmt.Errorf("failed to read varint: %w", err)
			}
			lengthSize += uint64(n)
			dataSize += sz
			if sz != 0 {
				nZeros = 0
			} else {
				nZeros++
			}
		}

		if info, ok := res.streamInfos[streamSection.StreamID]; ok {
			if res.freeSize == 0 || res.freeStart > info.offset-headerSize {
				res.freeStart = info.offset - headerSize
			}
			res.freeSize += headerSize + int64(info.size)
		}
		res.streamInfos[streamSection.StreamID] = streamInfo{
			offset: res.fileSize,
			size:   lengthSize + dataSize,
		}
		if _, err := buffer.Discard(int(dataSize)); err != nil {
			return nil, fmt.Errorf("failed to discard %d bytes: %w", dataSize, err)
		}
		res.fileSize += int64(lengthSize + dataSize)
	}
	if res.freeSize == 0 {
		res.freeStart = res.fileSize
	}

	// Keep the file pointer at the end of the file.
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return nil, err
	}

	return &res, nil
}

func (cachefile *cacheFile) StreamCount() uint64 {
	cachefile.rwmutex.RLock()
	defer cachefile.rwmutex.RUnlock()

	return uint64(len(cachefile.streamInfos))
}

func (cachefile *cacheFile) Reset() error {
	cachefile.rwmutex.Lock()
	defer cachefile.rwmutex.Unlock()

	if err := cachefile.file.Truncate(0); err != nil {
		return err
	}
	cachefile.streamInfos = map[uint64]streamInfo{}
	cachefile.freeSize = 0
	cachefile.fileSize = 0
	cachefile.freeStart = 0
	return nil
}

func (cachefile *cacheFile) Contains(streamID uint64) bool {
	cachefile.rwmutex.RLock()
	defer cachefile.rwmutex.RUnlock()

	_, ok := cachefile.streamInfos[streamID]
	return ok
}

func (cachefile *cacheFile) Data(streamID uint64) ([]index.Data, uint64, uint64, error) {
	cachefile.rwmutex.RLock()
	defer cachefile.rwmutex.RUnlock()

	info, ok := cachefile.streamInfos[streamID]
	if !ok {
		return nil, 0, 0, nil
	}

	buffer := bufio.NewReader(io.NewSectionReader(cachefile.file, info.offset, int64(info.size)))
	data := []index.Data{}

	type sizeAndDirection struct {
		Size      uint64
		Direction index.Direction
	}
	// Read chunk sizes
	dataSizes := []sizeAndDirection{}
	prevWasZero := false
	direction := index.DirectionClientToServer
	bytes := [2]uint64{0, 0}
	for {
		sz, _, err := readVarInt(buffer)
		if err != nil {
			return nil, 0, 0, err
		}
		if sz == 0 && prevWasZero {
			break
		}
		dataSizes = append(dataSizes, sizeAndDirection{Direction: direction, Size: sz})
		prevWasZero = sz == 0
		bytes[direction] += sz
		direction = direction.Reverse()
	}

	// Read data
	clientData := make([]byte, bytes[index.DirectionClientToServer])
	if _, err := io.ReadFull(buffer, clientData); err != nil {
		return nil, 0, 0, err
	}
	serverData := make([]byte, bytes[index.DirectionServerToClient])
	if _, err := io.ReadFull(buffer, serverData); err != nil {
		return nil, 0, 0, err
	}

	// Split data into chunks
	for _, ds := range dataSizes {
		if ds.Size == 0 {
			continue
		}
		var bytes []byte
		if ds.Direction == index.DirectionClientToServer {
			bytes = clientData[:ds.Size]
			clientData = clientData[ds.Size:]
		} else {
			bytes = serverData[:ds.Size]
			serverData = serverData[ds.Size:]
		}
		data = append(data, index.Data{
			Direction: ds.Direction,
			Content:   bytes,
		})
	}
	return data, bytes[index.DirectionClientToServer], bytes[index.DirectionServerToClient], nil
}

func (cachefile *cacheFile) DataForSearch(streamID uint64) ([2][]byte, [][2]int, uint64, uint64, error) {
	cachefile.rwmutex.RLock()
	defer cachefile.rwmutex.RUnlock()

	info, ok := cachefile.streamInfos[streamID]
	if !ok {
		return [2][]byte{}, [][2]int{}, 0, 0, nil
	}
	buffer := bufio.NewReader(io.NewSectionReader(cachefile.file, info.offset, int64(info.size)))

	// Read chunk sizes
	dataSizes := [][2]int{{}}
	prevWasZero := false
	direction := index.DirectionClientToServer
	clientBytes := uint64(0)
	serverBytes := uint64(0)
	for {
		last := dataSizes[len(dataSizes)-1]
		sz, _, err := readVarInt(buffer)
		if err != nil {
			return [2][]byte{}, [][2]int{}, 0, 0, err
		}
		if sz == 0 {
			if prevWasZero {
				break
			} else {
				prevWasZero = true
				direction = direction.Reverse()
				continue
			}
		}
		new := [2]int{
			last[0],
			last[1],
		}
		new[direction] += int(sz)
		dataSizes = append(dataSizes, new)
		prevWasZero = false
		if direction == index.DirectionClientToServer {
			clientBytes += sz
		} else {
			serverBytes += sz
		}
		direction = direction.Reverse()
	}

	// Read data
	clientData := make([]byte, clientBytes)
	if _, err := io.ReadFull(buffer, clientData); err != nil {
		return [2][]byte{}, [][2]int{}, 0, 0, err
	}
	serverData := make([]byte, serverBytes)
	if _, err := io.ReadFull(buffer, serverData); err != nil {
		return [2][]byte{}, [][2]int{}, 0, 0, err
	}
	return [2][]byte{clientData, serverData}, dataSizes, clientBytes, serverBytes, nil
}

func (cachefile *cacheFile) SetData(streamID uint64, convertedPackets []index.Data) error {
	cachefile.rwmutex.Lock()
	defer cachefile.rwmutex.Unlock()

	if info, ok := cachefile.streamInfos[streamID]; ok {
		cachefile.freeSize += int64(info.size) + headerSize
		if cachefile.freeStart > info.offset-headerSize {
			cachefile.freeStart = info.offset - headerSize
		}
		if cachefile.freeSize >= cleanupMinFreeSize && cachefile.freeSize >= int64(float64(cachefile.fileSize)*cleanupMinFreeFactor) {
			delete(cachefile.streamInfos, streamID)
			// cleanup the file
			if _, err := cachefile.file.Seek(cachefile.freeStart, io.SeekStart); err != nil {
				return err
			}

			reader := bufio.NewReader(io.NewSectionReader(cachefile.file, cachefile.freeStart, cachefile.fileSize-cachefile.freeStart))
			writer := bufio.NewWriter(cachefile.file)

			header := converterStreamSection{}
			for pos := cachefile.freeStart; ; {
				if err := binary.Read(reader, binary.LittleEndian, &header); err != nil {
					if err == io.EOF {
						break
					}
					return err
				}
				pos += headerSize
				if info := cachefile.streamInfos[streamID]; info.offset == pos {
					if err := binary.Write(writer, binary.LittleEndian, header); err != nil {
						return err
					}
					if _, err := io.CopyN(writer, reader, int64(info.size)); err != nil {
						return err
					}
					pos += int64(info.size)
					continue
				}
				dataSize := 0
				for nZeros := 0; nZeros != 2; {
					sz, n, err := readVarInt(reader)
					if err != nil {
						return err
					}
					dataSize += int(sz)
					pos += int64(n)
					if sz != 0 {
						nZeros = 0
					} else {
						nZeros++
					}
				}
				if _, err := reader.Discard(dataSize); err != nil {
					return err
				}
				pos += int64(dataSize)
			}

			if err := cachefile.file.Truncate(cachefile.fileSize); err != nil {
				return err
			}
			cachefile.freeSize = 0
		}
	}

	writer := bufio.NewWriter(cachefile.file)
	// Write stream header
	streamSection := converterStreamSection{
		StreamID: streamID,
	}
	if err := binary.Write(writer, binary.LittleEndian, &streamSection); err != nil {
		return err
	}

	streamSize := uint64(0)
	buf := [10]byte{}
	for pIndex, wantDir := 0, index.DirectionClientToServer; pIndex < len(convertedPackets); {
		// TODO: Merge packets with the same direction. Do we even want to allow converters to change the direction?
		convertedPacket := convertedPackets[pIndex]
		sz := len(convertedPacket.Content)
		dir := convertedPacket.Direction
		// Write a length of 0 if the server sent the first packet.
		if dir != wantDir {
			if err := writer.WriteByte(0); err != nil {
				return err
			}
			streamSize++
			wantDir = wantDir.Reverse()
		}
		pos := len(buf)
		flag := byte(0)
		for {
			pos--
			streamSize++
			buf[pos] = byte(sz&0x7f) | flag
			flag = 0x80
			sz >>= 7
			if sz == 0 {
				break
			}
		}
		if err := binary.Write(writer, binary.LittleEndian, buf[pos:]); err != nil {
			return err
		}
		wantDir = wantDir.Reverse()
		pIndex++
	}
	// Append two lengths of 0 to indicate the end of the chunk sizes
	if err := binary.Write(writer, binary.LittleEndian, []byte{0, 0}); err != nil {
		// TODO: The cache file is corrupt now. We should probably delete it.
		return err
	}
	streamSize += 2

	// Write chunk data
	for _, direction := range []index.Direction{index.DirectionClientToServer, index.DirectionServerToClient} {
		for _, convertedPacket := range convertedPackets {
			if convertedPacket.Direction != direction {
				continue
			}
			if err := binary.Write(writer, binary.LittleEndian, convertedPacket.Content); err != nil {
				return err
			}
			streamSize += uint64(len(convertedPacket.Content))
		}
	}

	if err := writer.Flush(); err != nil {
		return err
	}
	// TODO: Too slow to do every time, but we should do it after a while of not writing to the file.
	//       Otherwise the file is corrupted when stopping pkappa2, even if the last write was some time ago.
	// if err := cachefile.file.Sync(); err != nil {
	// 	return err
	// }

	// Remember where to look for this stream.
	cachefile.streamInfos[streamID] = streamInfo{
		offset: cachefile.fileSize + int64(unsafe.Sizeof(streamSection)),
		size:   streamSize,
	}

	if cachefile.freeStart == cachefile.fileSize {
		cachefile.freeStart += headerSize + int64(streamSize)
	}
	cachefile.fileSize += headerSize + int64(streamSize)

	return nil
}

// func (writer *writer) invalidateStream(stream *index.Stream) error {

// 	offset, ok := writer.cache.containedStreamIds[stream.ID()]
// 	if !ok {
// 		return nil
// 	}

// 	if err := writer.buffer.Flush(); err != nil {
// 		return err
// 	}
// 	if _, err := writer.file.Seek(offset, io.SeekStart); err != nil {
// 		return err
// 	}

// 	// Find stream in file and replace streamid with InvalidStreamID
// 	streamSection := converterStreamSection{}
// 	if err := binary.Read(writer.file, binary.LittleEndian, &streamSection); err != nil {
// 		return err
// 	}
// 	// Should never happen
// 	if streamSection.StreamID != stream.ID() {
// 		return fmt.Errorf("stream id mismatch during invalidation: %d != %d, offset %d", streamSection.StreamID, stream.ID(), offset)
// 	}

// 	streamSection.StreamID = InvalidStreamID
// 	if _, err := writer.file.Seek(-int64(unsafe.Sizeof(streamSection)), io.SeekCurrent); err != nil {
// 		return err
// 	}
// 	if err := binary.Write(writer.file, binary.LittleEndian, streamSection); err != nil {
// 		return err
// 	}

// 	delete(writer.cache.containedStreamIds, stream.ID())
// 	return nil
// }
