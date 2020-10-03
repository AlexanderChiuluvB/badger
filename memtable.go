/*
 * Copyright 2020 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package badger

import (
	"bufio"
	"bytes"
	"crypto/aes"
	cryptorand "crypto/rand"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"io/ioutil"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/dgraph-io/badger/v2/options"
	"github.com/dgraph-io/badger/v2/pb"
	"github.com/dgraph-io/badger/v2/skl"
	"github.com/dgraph-io/badger/v2/y"
	"github.com/dgraph-io/ristretto/z"
	"github.com/pkg/errors"
)

// Also, memTable should have a way to open a WAL and bring SkipList up to speed.
// On start, if there's a logfile, then create corresponding skiplist and create memtable struct.
type memTable struct {
	// Give skiplist z.Calloc'd []byte.
	sl        *skl.Skiplist
	wal       *logFile
	ref       int32
	nextTxnTs uint64
	opt       Options
	buf       *bytes.Buffer
}

func (db *DB) openMemTables(opt Options) error {
	files, err := ioutil.ReadDir(db.opt.Dir)
	if err != nil {
		return errFile(err, db.opt.Dir, "Unable to open mem dir.")
	}

	var fids []int
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), memFileExt) {
			continue
		}
		fsz := len(file.Name())
		fid, err := strconv.ParseInt(file.Name()[:fsz-len(memFileExt)], 10, 64)
		if err != nil {
			return errFile(err, file.Name(), "Unable to parse log id.")
		}
		fids = append(fids, int(fid))
	}

	// Sort in ascending order.
	sort.Slice(fids, func(i, j int) bool {
		return fids[i] < fids[j]
	})
	for _, fid := range fids {
		mt, err := db.openMemTable(fid)
		if err != nil {
			return err
		}
		// These should no longer be written to. So, make them part of the imm.
		// TODO: We need the max version from the skiplist.
		db.imm = append(db.imm, mt)
	}
	if len(fids) != 0 {
		db.nextMemFid = fids[len(fids)-1]
	}
	db.nextMemFid++
	return nil
}

const memFileExt string = ".mem"

func (db *DB) openMemTable(fid int) (*memTable, error) {
	filepath := db.mtFilePath(fid)
	lf := &logFile{
		fid:         uint32(fid),
		path:        filepath,
		loadingMode: options.MemoryMap,
		registry:    db.registry,
		writeAt:     vlogHeaderSize,
	}
	lerr := lf.open(filepath, os.O_RDWR|os.O_CREATE, db.opt)
	if lerr != z.NewFile && lerr != nil {
		return nil, errors.Wrapf(lerr, "While opening mem table")
	}
	s := skl.NewSkiplist(arenaSize(db.opt))
	mt := &memTable{
		wal: lf,
		sl:  s,
		ref: 1,
		opt: db.opt,
	}
	if lerr == z.NewFile {
		return mt, nil
	}
	err := mt.UpdateSkipList()
	return mt, err
}

func (db *DB) newMemTable() (*memTable, error) {
	mt, err := db.openMemTable(db.nextMemFid)
	db.nextMemFid++
	return mt, err
}

func (db *DB) mtFilePath(fid int) string {
	return path.Join(db.opt.Dir, fmt.Sprintf("%05d%s", fid, memFileExt))
}

func (mt *memTable) Put(key []byte, value y.ValueStruct) error {
	entry := &Entry{
		Key:       key,
		Value:     value.Value,
		UserMeta:  value.UserMeta,
		meta:      value.Meta,
		ExpiresAt: value.ExpiresAt,
	}
	if err := mt.wal.writeEntry(mt.buf, entry, mt.opt); err != nil {
		return errors.Wrapf(err, "cannot write entry to WAL file")
	}
	mt.sl.Put(key, value)
	return nil
}

func (mt *memTable) UpdateSkipList() error {
	if mt.wal == nil || mt.sl == nil {
		return nil
	}
	endOff, err := mt.wal.iterate(true, 0, mt.replayFunction(mt.opt))
	if err != nil {
		return errors.Wrapf(err, "while iterating wal: %s", mt.wal.Fd.Name())
	}

	// TODO: Figure out whether the Truncate option should be respected or not.
	return mt.wal.Truncate(int64(endOff))
}

// IncrRef increases the refcount
func (mt *memTable) IncrRef() {
	atomic.AddInt32(&mt.ref, 1)
}

// DecrRef decrements the refcount, deallocating the Skiplist when done using it
func (mt *memTable) DecrRef() {
	newRef := atomic.AddInt32(&mt.ref, -1)
	if newRef > 0 {
		return
	}

	mt.sl.ReclaimMem()
	mt.wal.Delete()
}

func (mt *memTable) replayFunction(opt Options) func(Entry, valuePointer) error {
	first := true
	return func(e Entry, _ valuePointer) error { // Function for replaying.
		if first {
			opt.Debugf("First key=%q\n", e.Key)
		}
		first = false
		if ts := y.ParseTs(e.Key); ts > mt.nextTxnTs {
			mt.nextTxnTs = ts
		}
		v := y.ValueStruct{
			Value:     e.Value,
			Meta:      e.meta,
			UserMeta:  e.UserMeta,
			ExpiresAt: e.ExpiresAt,
		}
		// This is already encoded correctly. Value would be either a vptr, or a full value
		// depending upon how big the original value was.
		// Skiplist makes a copy of the key and value.
		mt.sl.Put(e.Key, v)
		return nil
	}
}

type logFile struct {
	*z.MmapFile
	path string
	// This is a lock on the log file. It guards the fd’s value, the file’s
	// existence and the file’s memory map.
	//
	// Use shared ownership when reading/writing the file or memory map, use
	// exclusive ownership to open/close the descriptor, unmap or remove the file.
	lock        sync.RWMutex
	fid         uint32
	size        uint32
	loadingMode options.FileLoadingMode
	dataKey     *pb.DataKey
	baseIV      []byte
	registry    *KeyRegistry
	writeAt     uint32
}

// encodeEntry will encode entry to the buf
// layout of entry
// +--------+-----+-------+-------+
// | header | key | value | crc32 |
// +--------+-----+-------+-------+
func (lf *logFile) encodeEntry(buf *bytes.Buffer, e *Entry, offset uint32) (int, error) {
	h := header{
		klen:      uint32(len(e.Key)),
		vlen:      uint32(len(e.Value)),
		expiresAt: e.ExpiresAt,
		meta:      e.meta,
		userMeta:  e.UserMeta,
	}

	hash := crc32.New(y.CastagnoliCrcTable)
	writer := io.MultiWriter(buf, hash)

	// encode header.
	var headerEnc [maxHeaderSize]byte
	sz := h.Encode(headerEnc[:])
	y.Check2(writer.Write(headerEnc[:sz]))
	// we'll encrypt only key and value.
	if lf.encryptionEnabled() {
		// TODO: no need to allocate the bytes. we can calculate the encrypted buf one by one
		// since we're using ctr mode of AES encryption. Ordering won't changed. Need some
		// refactoring in XORBlock which will work like stream cipher.
		eBuf := make([]byte, 0, len(e.Key)+len(e.Value))
		eBuf = append(eBuf, e.Key...)
		eBuf = append(eBuf, e.Value...)
		if err := y.XORBlockStream(
			writer, eBuf, lf.dataKey.Data, lf.generateIV(offset)); err != nil {
			return 0, y.Wrapf(err, "Error while encoding entry for vlog.")
		}
	} else {
		// Encryption is disabled so writing directly to the buffer.
		y.Check2(writer.Write(e.Key))
		y.Check2(writer.Write(e.Value))
	}
	// write crc32 hash.
	var crcBuf [crc32.Size]byte
	binary.BigEndian.PutUint32(crcBuf[:], hash.Sum32())
	y.Check2(buf.Write(crcBuf[:]))
	// return encoded length.
	return len(headerEnc[:sz]) + len(e.Key) + len(e.Value) + len(crcBuf), nil
}

func (lf *logFile) writeEntry(buf *bytes.Buffer, e *Entry, opt Options) error {
	buf.Reset()
	plen, err := lf.encodeEntry(buf, e, lf.writeAt)
	if err != nil {
		return err
	}
	y.AssertTrue(plen == copy(lf.Data[lf.writeAt:], buf.Bytes()))
	lf.writeAt += uint32(plen)
	return nil
}

func (lf *logFile) decodeEntry(buf []byte, offset uint32) (*Entry, error) {
	var h header
	hlen := h.Decode(buf)
	kv := buf[hlen:]
	if lf.encryptionEnabled() {
		var err error
		// No need to worry about mmap. because, XORBlock allocates a byte array to do the
		// xor. So, the given slice is not being mutated.
		if kv, err = lf.decryptKV(kv, offset); err != nil {
			return nil, err
		}
	}
	e := &Entry{
		meta:      h.meta,
		UserMeta:  h.userMeta,
		ExpiresAt: h.expiresAt,
		offset:    offset,
		Key:       kv[:h.klen],
		Value:     kv[h.klen : h.klen+h.vlen],
	}
	return e, nil
}

func (lf *logFile) decryptKV(buf []byte, offset uint32) ([]byte, error) {
	return y.XORBlockAllocate(buf, lf.dataKey.Data, lf.generateIV(offset))
}

// KeyID returns datakey's ID.
func (lf *logFile) keyID() uint64 {
	if lf.dataKey == nil {
		// If there is no datakey, then we'll return 0. Which means no encryption.
		return 0
	}
	return lf.dataKey.KeyId
}

func (lf *logFile) mmap(size int64) (err error) {
	if lf.loadingMode != options.MemoryMap {
		// Nothing to do
		return nil
	}

	// Increase the file size so that mmap doesn't complain.
	if err := lf.Fd.Truncate(size); err != nil {
		return err
	}

	lf.Data, err = y.Mmap(lf.Fd, true, size)
	if err == nil {
		err = y.Madvise(lf.Data, false) // Disable readahead
	}
	return err
}

func (lf *logFile) encryptionEnabled() bool {
	return lf.dataKey != nil
}

func (lf *logFile) munmap() (err error) {
	if lf.loadingMode != options.MemoryMap || len(lf.Data) == 0 {
		// Nothing to do
		return nil
	}

	if err := y.Munmap(lf.Data); err != nil {
		return errors.Wrapf(err, "Unable to munmap value log: %q", lf.path)
	}
	// This is important. We should set the map to nil because ummap
	// system call doesn't change the length or capacity of the fmap slice.
	lf.Data = nil
	return nil
}

// Acquire lock on mmap/file if you are calling this
func (lf *logFile) read(p valuePointer, s *y.Slice) (buf []byte, err error) {
	var nbr int64
	offset := p.Offset
	if lf.loadingMode == options.FileIO {
		buf = s.Resize(int(p.Len))
		var n int
		n, err = lf.Fd.ReadAt(buf, int64(offset))
		nbr = int64(n)
	} else {
		// Do not convert size to uint32, because the lf.Data can be of size
		// 4GB, which overflows the uint32 during conversion to make the size 0,
		// causing the read to fail with ErrEOF. See issue #585.
		size := int64(len(lf.Data))
		valsz := p.Len
		lfsz := atomic.LoadUint32(&lf.size)
		if int64(offset) >= size || int64(offset+valsz) > size ||
			// Ensure that the read is within the file's actual size. It might be possible that
			// the offset+valsz length is beyond the file's actual size. This could happen when
			// dropAll and iterations are running simultaneously.
			int64(offset+valsz) > int64(lfsz) {
			err = y.ErrEOF
		} else {
			buf = lf.Data[offset : offset+valsz]
			nbr = int64(valsz)
		}
	}
	y.NumReads.Add(1)
	y.NumBytesRead.Add(nbr)
	return buf, err
}

// generateIV will generate IV by appending given offset with the base IV.
func (lf *logFile) generateIV(offset uint32) []byte {
	iv := make([]byte, aes.BlockSize)
	// baseIV is of 12 bytes.
	y.AssertTrue(12 == copy(iv[:12], lf.baseIV))
	// remaining 4 bytes is obtained from offset.
	binary.BigEndian.PutUint32(iv[12:], offset)
	return iv
}

func (lf *logFile) doneWriting(offset uint32) error {
	// Just always sync on rotate.
	if err := z.Msync(lf.Data); err != nil {
		return errors.Wrapf(err, "Unable to sync value log: %q", lf.path)
	}

	// Before we were acquiring a lock here on lf.lock, because we were invalidating the file
	// descriptor due to reopening it as read-only. Now, we don't invalidate the fd, but unmap it,
	// truncate it and remap it. That creates a window where we have segfaults because the mmap is
	// no longer valid, while someone might be reading it. Therefore, we need a lock here again.
	lf.lock.Lock()
	defer lf.lock.Unlock()

	// Unmap file before we truncate it. Windows cannot truncate a file that is mmapped.
	if err := lf.munmap(); err != nil {
		return errors.Wrapf(err, "failed to munmap vlog file %s", lf.Fd.Name())
	}

	// TODO: Confirm if we need to run a file sync after truncation.
	// Truncation must run after unmapping, otherwise Windows would crap itself.
	if err := lf.Fd.Truncate(int64(offset)); err != nil {
		return errors.Wrapf(err, "Unable to truncate file: %q", lf.path)
	}

	// Reinitialize the log file. This will mmap the entire file.
	if err := lf.init(); err != nil {
		return errors.Wrapf(err, "failed to initialize file %s", lf.Fd.Name())
	}

	// Previously we used to close the file after it was written and reopen it in read-only mode.
	// We no longer open files in read-only mode. We keep all vlog files open in read-write mode.
	return nil
}

// You must hold lf.lock to sync()
func (lf *logFile) sync() error {
	return z.Msync(lf.Data)
}

// iterate iterates over log file. It doesn't not allocate new memory for every kv pair.
// Therefore, the kv pair is only valid for the duration of fn call.
func (lf *logFile) iterate(readOnly bool, offset uint32, fn logEntry) (uint32, error) {
	if offset == 0 {
		// If offset is set to zero, let's advance past the encryption key header.
		offset = vlogHeaderSize
	}
	// TODO: Don't know what the end of file is. We just have to read it to know it.
	// if readOnly {
	// 	// We're not at the end of the file. We'd need to replay the entries, or
	// 	// possibly truncate the file.
	// 	return 0, ErrReplayNeeded
	// }

	// For now, read directly from file, because it allows
	reader := bufio.NewReader(lf.NewReader(int(offset)))
	read := &safeRead{
		k:            make([]byte, 10),
		v:            make([]byte, 10),
		recordOffset: offset,
		lf:           lf,
	}

	var lastCommit uint64
	var validEndOffset uint32 = offset

	var entries []*Entry
	var vptrs []valuePointer

loop:
	for {
		e, err := read.Entry(reader)
		switch {
		// We have not reached the end of the file but the entry we read is
		// zero. This happens because we have truncated the file and
		// zero'ed it out.
		case err == io.EOF || e.isZero():
			break loop
		case err == io.ErrUnexpectedEOF || err == errTruncate:
			break loop
		case err != nil:
			return 0, err
		case e == nil:
			continue
		}

		var vp valuePointer
		vp.Len = uint32(int(e.hlen) + len(e.Key) + len(e.Value) + crc32.Size)
		read.recordOffset += vp.Len

		vp.Offset = e.offset
		vp.Fid = lf.fid

		switch {
		case e.meta&bitTxn > 0:
			txnTs := y.ParseTs(e.Key)
			if lastCommit == 0 {
				lastCommit = txnTs
			}
			if lastCommit != txnTs {
				break loop
			}
			entries = append(entries, e)
			vptrs = append(vptrs, vp)

		case e.meta&bitFinTxn > 0:
			txnTs, err := strconv.ParseUint(string(e.Value), 10, 64)
			if err != nil || lastCommit != txnTs {
				break loop
			}
			// Got the end of txn. Now we can store them.
			lastCommit = 0
			validEndOffset = read.recordOffset

			for i, e := range entries {
				vp := vptrs[i]
				if err := fn(*e, vp); err != nil {
					if err == errStop {
						break
					}
					return 0, errFile(err, lf.path, "Iteration function")
				}
			}
			entries = entries[:0]
			vptrs = vptrs[:0]

		default:
			if lastCommit != 0 {
				// This is most likely an entry which was moved as part of GC.
				// We shouldn't get this entry in the middle of a transaction.
				break loop
			}
			validEndOffset = read.recordOffset

			if err := fn(*e, vp); err != nil {
				if err == errStop {
					break
				}
				return 0, errFile(err, lf.path, "Iteration function")
			}
		}
	}
	return validEndOffset, nil
}

func (lf *logFile) open(path string, flags int, opt Options) error {
	mf, ferr := z.OpenMmapFile(path, flags, 2*int(opt.ValueLogFileSize))
	lf.MmapFile = mf
	if ferr == z.NewFile {
		if err := lf.bootstrap(); err != nil {
			os.Remove(path)
			return err
		}
	} else if ferr != nil {
		return errors.Wrapf(ferr, "while opening file: %s", path)
	}

	// if sz < vlogHeaderSize {
	// 	// Every vlog file should have at least vlogHeaderSize. If it is less than vlogHeaderSize
	// 	// then it must have been corrupted. But no need to handle here. log replayer will truncate
	// 	// and bootstrap the logfile. So ignoring here.
	// 	return nil
	// }

	// Copy over the encryption registry data.
	buf := make([]byte, vlogHeaderSize)

	y.AssertTrue(vlogHeaderSize == copy(buf, lf.Data))
	keyID := binary.BigEndian.Uint64(buf[:8])
	// retrieve datakey.
	if dk, err := lf.registry.dataKey(keyID); err != nil {
		return y.Wrapf(err, "While opening vlog file %d", lf.fid)
	} else {
		lf.dataKey = dk
	}
	lf.baseIV = buf[8:]
	y.AssertTrue(len(lf.baseIV) == 12)

	// Preserved ferr so we can return if this was a new file.
	return ferr
}

// bootstrap will initialize the log file with key id and baseIV.
// The below figure shows the layout of log file.
// +----------------+------------------+------------------+
// | keyID(8 bytes) |  baseIV(12 bytes)|	 entry...     |
// +----------------+------------------+------------------+
func (lf *logFile) bootstrap() error {
	var err error

	// generate data key for the log file.
	var dk *pb.DataKey
	if dk, err = lf.registry.latestDataKey(); err != nil {
		return y.Wrapf(err, "Error while retrieving datakey in logFile.bootstarp")
	}
	lf.dataKey = dk

	// We'll always preserve vlogHeaderSize for key id and baseIV.
	buf := make([]byte, vlogHeaderSize)

	// write key id to the buf.
	// key id will be zero if the logfile is in plain text.
	binary.BigEndian.PutUint64(buf[:8], lf.keyID())
	// generate base IV. It'll be used with offset of the vptr to encrypt the entry.
	if _, err := cryptorand.Read(buf[8:]); err != nil {
		return y.Wrapf(err, "Error while creating base IV, while creating logfile")
	}

	// Initialize base IV.
	lf.baseIV = buf[8:]
	y.AssertTrue(len(lf.baseIV) == 12)

	// Copy over to the logFile.
	y.AssertTrue(vlogHeaderSize == copy(lf.Data[0:], buf))
	return nil
}

func (lf *logFile) reset() {
	// TODO: This is needed in in-memory mode.
	if lf == nil {
		return
	}
	z.ZeroOut(lf.Data, vlogHeaderSize, int(lf.writeAt))
	lf.writeAt = vlogHeaderSize
}
