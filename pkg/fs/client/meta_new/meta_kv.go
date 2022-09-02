/*
Copyright (c) 2022 PaddlePaddle Authors. All Rights Reserve.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package meta_new

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	pathlib "path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/PaddlePaddle/PaddleFlow/pkg/fs/client/base"
	"github.com/PaddlePaddle/PaddleFlow/pkg/fs/client/kv_new"
	ufslib "github.com/PaddlePaddle/PaddleFlow/pkg/fs/client/ufs"
	"github.com/PaddlePaddle/PaddleFlow/pkg/fs/client/utils"
	"github.com/PaddlePaddle/PaddleFlow/pkg/fs/common"
)

const (
	AttrKey  = "A"
	EntryKey = "E"
	InodeKey = "I"
	// inodeSize struct size
	inodeSize = 105
	// entrySize struct size
	entrySize = 21
	entryDone = 1

	rootInodeID = Ino(1)

	MemDriver    = "mem"
	DiskDriver   = "disk"
	nextInodeKey = "nextInodeKey"
)

var _ Meta = &kvMeta{}

// kvMeta
type kvMeta struct {
	client       kv_new.KvClient
	attrTimeOut  time.Duration
	entryTimeOut time.Duration
	defaultUfs   ufslib.UnderFileStorage
	ufsMap       *sync.Map
	ufsMapLock   sync.RWMutex
	setOwner     bool
	uid          uint32
	gid          uint32
}

type entryItem struct {
	ino    Ino
	mode   uint32
	expire int64
	// if done is true, means the parent entry for readdir cache is effective.
	done uint8
}

type inodeItem struct {
	attr      Attr
	parentIno Ino
	expire    int64
	name      []byte
}

func newKvMeta(fsMeta common.FSMeta, links map[string]common.FSMeta, config Config) (Meta, error) {
	client, err := newClient(config.Config)
	if err != nil {
		return nil, err
	}
	m := &kvMeta{
		client:       client,
		attrTimeOut:  config.AttrCacheExpire,
		entryTimeOut: config.EntryCacheExpire,
	}
	ufs, err := newUFS(fsMeta)
	if err != nil {
		return nil, err
	}
	m.defaultUfs = ufs
	err = m.UpdateUFSMap(links)
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (m *kvMeta) UpdateUFSMap(fsMetas map[string]common.FSMeta) error {
	var ufsMap sync.Map
	for key, value := range fsMetas {
		linkUfs, err := newUFS(value)
		if err != nil {
			log.Errorf("new ufs for fsMeta[%+v] failed: %v", value, err)
			return err
		}
		ufsMap.Store(pathlib.Clean(key), linkUfs)
	}
	m.ufsMapLock.Lock()
	m.ufsMap = &ufsMap
	m.ufsMapLock.Unlock()
	return nil
}

func newUFS(fsMeta common.FSMeta) (ufslib.UnderFileStorage, error) {
	log.Debugf("begin to new UFS: fsMeta[%+v]", fsMeta)
	properties := make(map[string]interface{})
	for k, v := range fsMeta.Properties {
		properties[k] = v
	}
	properties[common.SubPath] = fsMeta.SubPath
	properties[common.Type] = fsMeta.UfsType

	switch fsMeta.UfsType {
	case common.HDFSType:
		properties[common.NameNodeAddress] = fsMeta.ServerAddress
	case common.HDFSWithKerberosType:
		properties[common.NameNodeAddress] = fsMeta.ServerAddress
	case common.SFTPType, common.CFSType, common.GlusterFSType:
		properties[common.Address] = fsMeta.ServerAddress
	}
	return ufslib.NewUFS(fsMeta.UfsType, properties)
}

func newClient(config kv_new.Config) (kv_new.KvClient, error) {
	var client kv_new.KvClient
	var err error
	switch config.Driver {
	case kv_new.DiskType, kv_new.MemType:
		client, err = kv_new.NewBadgerClient(config)
	default:
		return nil, fmt.Errorf("unknown meta client")
	}
	return client, err
}

func (m *kvMeta) fmtKey(args ...interface{}) []byte {
	b := utils.NewBuffer(uint32(m.keyLen(args...)))
	for _, a := range args {
		switch a := a.(type) {
		case byte:
			b.Put8(a)
		case uint32:
			b.Put32(a)
		case uint64:
			b.Put64(a)
		case Ino:
			m.encodeInode(a, b.Get(8))
		case string:
			b.Put([]byte(a))
		default:
			log.Panicf("invalid type %T, value %v", a, a)
		}
	}
	return b.Bytes()
}

func (m *kvMeta) keyLen(args ...interface{}) int {
	var c int
	for _, a := range args {
		switch a := a.(type) {
		case byte:
			c++
		case uint32:
			c += 4
		case uint64:
			c += 8
		case Ino:
			c += 8
		case string:
			c += len(a)
		default:
			log.Panicf("invalid type %T, value %v", a, a)
		}
	}
	return c
}

func (m *kvMeta) encodeInode(ino Ino, buf []byte) {
	binary.LittleEndian.PutUint64(buf, uint64(ino))
}

func (m *kvMeta) counterKey(key string) []byte {
	return m.fmtKey("C", key)
}

func (m *kvMeta) inodeKey(ino Ino) []byte {
	return m.fmtKey("I", ino)
}

func (m *kvMeta) entryKey(parent Ino, name string) []byte {
	return m.fmtKey("E", parent, "N", name)
}

func (m *kvMeta) get(key []byte) ([]byte, error) {
	var value []byte
	err := m.client.Txn(func(tx kv_new.KvTxn) error {
		value = tx.Get(key)
		return nil
	})
	return value, err
}

func (m *kvMeta) set(key []byte, value []byte) error {
	err := m.client.Txn(func(tx kv_new.KvTxn) error {
		errTx := tx.Set(key, value)
		return errTx
	})
	return err
}

func (m *kvMeta) scanValues(prefix []byte) (map[string][]byte, error) {
	var values map[string][]byte
	err := m.client.Txn(func(tx kv_new.KvTxn) error {
		values, _ = tx.ScanValues(prefix)
		return nil
	})
	return values, err
}

func (m *kvMeta) nextInode() (Ino, error) {
	var value int64
	err := m.client.Txn(func(tx kv_new.KvTxn) error {
		value = tx.NextNumber()
		return nil
	})

	return Ino(value), err
}

func (m *kvMeta) parseInode(buf []byte, inode *inodeItem) {
	if inode == nil {
		return
	}
	rb := utils.FromBuffer(buf)
	inode.attr.Type = rb.Get8()
	inode.attr.Mode = rb.Get32()
	inode.attr.Uid = rb.Get32()
	inode.attr.Gid = rb.Get32()
	inode.attr.Rdev = rb.Get64()
	inode.attr.Atime = int64(rb.Get64())
	inode.attr.Mtime = int64(rb.Get64())
	inode.attr.Ctime = int64(rb.Get64())
	inode.attr.Atimensec = rb.Get32()
	inode.attr.Mtimensec = rb.Get32()
	inode.attr.Ctimensec = rb.Get32()
	inode.attr.Nlink = rb.Get64()
	inode.attr.Size = rb.Get64()
	inode.attr.Blksize = int64(rb.Get64())
	inode.attr.Block = int64(rb.Get64())
	inode.parentIno = Ino(rb.Get64())
	inode.expire = int64(rb.Get64())
	inode.name = rb.Get(rb.Left())
}

func (m *kvMeta) marshalInode(inode *inodeItem) []byte {
	w := utils.NewBuffer(inodeSize + uint32(len(inode.name)))
	w.Put8(inode.attr.Type)
	w.Put32(inode.attr.Mode)
	w.Put32(inode.attr.Uid)
	w.Put32(inode.attr.Gid)
	w.Put64(inode.attr.Rdev)
	w.Put64(uint64(inode.attr.Atime))
	w.Put64(uint64(inode.attr.Mtime))
	w.Put64(uint64(inode.attr.Ctime))
	w.Put32(inode.attr.Atimensec)
	w.Put32(inode.attr.Mtimensec)
	w.Put32(inode.attr.Ctimensec)
	w.Put64(inode.attr.Nlink)
	w.Put64(inode.attr.Size)
	w.Put64(uint64(inode.attr.Blksize))
	w.Put64(uint64(inode.attr.Block))
	w.Put64(uint64(inode.parentIno))
	w.Put64(uint64(inode.expire))
	w.Put(inode.name)
	return w.Bytes()
}

func (m *kvMeta) parseEntry(buf []byte, entry *entryItem) {
	if entry == nil {
		return
	}
	rb := utils.FromBuffer(buf)
	entry.ino = Ino(rb.Get64())
	entry.mode = rb.Get32()
	entry.expire = int64(rb.Get64())
	entry.done = rb.Get8()
}

func (m *kvMeta) marshalEntry(entry *entryItem) []byte {
	w := utils.NewBuffer(entrySize)
	w.Put64(uint64(entry.ino))
	w.Put32(entry.mode)
	w.Put64(uint64(entry.expire))
	w.Put8(entry.done)
	return w.Bytes()
}

func (m *kvMeta) fullPath(inode Ino) string {
	var builder strings.Builder
	var segments []string
	if inode == rootInodeID {
		return "/"
	}
	for {
		if inode == rootInodeID {
			break
		}
		d, err := m.get(m.inodeKey(inode))
		if err != nil {
			panic(err)
		}
		if d == nil {
			panic(fmt.Sprintf("full path parse fail with inode %v", inode))
		}
		attr := &inodeItem{}
		m.parseInode(d, attr)
		inode = attr.parentIno
		segments = append(segments, string(attr.name))
	}
	for i := len(segments) - 1; i >= 0; i-- {
		builder.WriteString("/")
		builder.WriteString(segments[i])
	}
	return builder.String()
}

func (m *kvMeta) txn(f func(tx kv_new.KvTxn) error) error {
	var err error
	for i := 0; i < 50; i++ {
		if err = m.client.Txn(f); m.shouldRetry(err) {
			time.Sleep(time.Millisecond * time.Duration(i*i))
			continue
		}
		break
	}
	return err
}

func (m *kvMeta) shouldRetry(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := err.(syscall.Errno); ok {
		return false
	}
	// TODO: add other retryable errors here
	return strings.Contains(err.Error(), "write conflict") || strings.Contains(err.Error(), "TxnLockNotFound")
}

func (m *kvMeta) getAttrFromCache(ino Ino, attr *inodeItem) bool {
	a, err := m.get(m.inodeKey(ino))
	if err != nil || a == nil {
		return false
	}
	m.parseInode(a, attr)
	if attr.expire < time.Now().Unix() {
		return false
	}
	return true
}

func (m *kvMeta) GetUFS(name string) (ufslib.UnderFileStorage, bool, string, string) {
	var findUfs ufslib.UnderFileStorage
	findKey := ""
	targetPath := pathlib.Clean(pathlib.Join("/", name))
	newPath := name
	// todo：改为并发安全的container/list实现
	m.ufsMapLock.RLock()
	ufsMap := m.ufsMap
	m.ufsMapLock.RUnlock()
	if ufsMap != nil {
		ufsMap.Range(func(key, value interface{}) bool {
			prefix := key.(string)
			// "/"要加到Clean外面，这样确保匹配的前缀是目录项
			keyPath := pathlib.Clean(pathlib.Join("/", prefix)) + "/"

			if strings.HasPrefix(targetPath, keyPath) || targetPath+"/" == keyPath {
				findUfs = value.(ufslib.UnderFileStorage)
				findKey = keyPath
				if targetPath+"/" == keyPath {
					newPath = ""
				} else {
					newPath = strings.TrimPrefix(targetPath, keyPath)
				}
				return false
			}
			return true
		})
	}
	isLink := true
	if findUfs == nil {
		isLink = false
		findUfs = m.defaultUfs
		findKey = ""
	}

	log.Debugf("source path[%s], findKey[%s], isLink[%t], path[%s]", name, findKey, isLink, newPath)
	return findUfs, isLink, findKey, newPath
}

func (m *kvMeta) Name() string {
	return "meta_kv"
}

func (m *kvMeta) InitRootInode() error {
	attr := &Attr{}
	now := time.Now()
	attr.Type = TypeDirectory
	attr.Mode = syscall.S_IFDIR | 0777
	attr.Nlink = 2
	attr.Size = 4 << 10
	attr.Mtime = now.Unix()
	attr.Mtimensec = uint32(now.Nanosecond())
	attr.Ctime = now.Unix()
	attr.Ctimensec = uint32(now.Nanosecond())
	inodeItem_ := &inodeItem{
		parentIno: 0,
		name:      []byte("/"),
		expire:    now.Add(1000 * time.Hour).Unix(), // root inode not expire
		attr:      *attr,
	}
	err := m.set(m.inodeKey(rootInodeID), m.marshalInode(inodeItem_))
	return err
}

func (m *kvMeta) SetOwner(uid, gid uint32) {
}

func (m *kvMeta) StatFS(ctx *Context) (*base.StatfsOut, syscall.Errno) {
	panic("implement me")
}

func (m *kvMeta) Access(ctx *Context, inode Ino, mask uint32, attr *Attr) syscall.Errno {
	if ctx.Uid == 0 {
		return 0
	}

	if attr == nil {
		attr = &Attr{}
		err := m.GetAttr(ctx, inode, attr)
		if utils.IsError(err) {
			return err
		}
	}

	uid := attr.Uid
	gid := attr.Gid
	if m.setOwner {
		uid = m.uid
		gid = m.gid
	}
	if !utils.HasAccess(ctx.Uid, ctx.Gid, uid, gid, attr.Mode, mask) {
		return syscall.EACCES
	}
	return syscall.F_OK
}

func (m *kvMeta) Lookup(ctx *Context, parent Ino, name string) (Ino, *Attr, syscall.Errno) {
	entry, err := m.get(m.entryKey(parent, name))
	if err != nil {
		return 0, nil, syscall.EIO
	}
	var inode Ino
	attr := &Attr{}
	if entry != nil {
		entryItem_ := &entryItem{}
		m.parseEntry(entry, entryItem_)
		inode = entryItem_.ino
		inodeItem_ := &inodeItem{}
		ok := m.getAttrFromCache(inode, inodeItem_)
		if ok {
			*attr = inodeItem_.attr
			return inode, attr, syscall.F_OK
		}
	}

	absolutePath := filepath.Join(m.fullPath(parent), name)
	ufs, isLink, prefix, path := m.GetUFS(absolutePath)
	info, err := ufs.GetAttr(path)
	now := time.Now()
	if err != nil {
		log.Debugf("[vfs-lookup] GetAttr failed: %v with path[%s] name[%s] and absolutePath[%s]", err, path, name, absolutePath)
		return 0, nil, utils.ToSyscallErrno(err)
	}
	if isLink {
		info.FixLinkPrefix(prefix)
	}
	attr.FromFileInfo(info)

	err = m.txn(func(tx kv_new.KvTxn) error {
		if entry == nil {
			number, err := m.client.NextNumber([]byte(nextInodeKey))
			if err != nil {
				return err
			}
			inode = Ino(number)
			entryItem_ := &entryItem{
				ino:  inode,
				mode: attr.Mode,
			}
			err = tx.Set(m.entryKey(parent, name), m.marshalEntry(entryItem_))
			if err != nil {
				return err
			}
		}
		inodeItem_ := &inodeItem{
			expire:    now.Add(m.attrTimeOut).Unix(),
			attr:      *attr,
			parentIno: parent,
			name:      []byte(name),
		}
		log.Debugf("setattr inode %v %v", inode, inodeItem_)

		err = tx.Set(m.inodeKey(inode), m.marshalInode(inodeItem_))
		return err
	})
	return inode, attr, utils.ToSyscallErrno(err)
}

func (m *kvMeta) Resolve(ctx *Context, parent Ino, path string, inode *Ino, attr *Attr) syscall.Errno {
	panic("implement me")
}

func (m *kvMeta) GetAttr(ctx *Context, inode Ino, attr *Attr) syscall.Errno {
	inodeItem_ := &inodeItem{}
	if inode == rootInodeID {
		now := time.Now()
		attr.Type = TypeDirectory
		attr.Mode = syscall.S_IFDIR | 0777
		attr.Nlink = 2
		attr.Size = 4 << 10
		attr.Mtime = now.Unix()
		attr.Mtimensec = uint32(now.Nanosecond())
		attr.Ctime = now.Unix()
		attr.Ctimensec = uint32(now.Nanosecond())
		return syscall.F_OK
	}
	buf, err := m.get(m.inodeKey(inode))
	if err != nil {
		return syscall.EIO
	}
	if buf == nil {
		log.Debugf("get attr is nod found %v", attr)
		return syscall.ENOENT
	}
	m.parseInode(buf, inodeItem_)
	*attr = inodeItem_.attr
	return syscall.F_OK
}

func (m *kvMeta) SetAttr(ctx *Context, inode Ino, set uint32, attr *Attr) syscall.Errno {
	panic("implement me")
}

func (m *kvMeta) Truncate(ctx *Context, inode Ino, size uint64) syscall.Errno {
	panic("implement me")
}

func (m *kvMeta) Fallocate(ctx *Context, inode Ino, mode uint8, off uint64, size uint64) syscall.Errno {
	panic("implement me")
}

func (m *kvMeta) ReadLink(ctx *Context, inode Ino, path *[]byte) syscall.Errno {
	panic("implement me")
}

func (m *kvMeta) Symlink(ctx *Context, parent Ino, name string, path string, inode *Ino, attr *Attr) syscall.Errno {
	panic("implement me")
}

func (m *kvMeta) Mknod(ctx *Context, parent Ino, name string, mode uint32, rdev uint32, inode *Ino, attr *Attr) syscall.Errno {
	panic("implement me")
}

func (m *kvMeta) Mkdir(ctx *Context, parent Ino, name string, mode uint32, inode *Ino, attr *Attr) syscall.Errno {
	panic("implement me")
}

func (m *kvMeta) Unlink(ctx *Context, parent Ino, name string) syscall.Errno {
	panic("implement me")
}

func (m *kvMeta) Rmdir(ctx *Context, parent Ino, name string) syscall.Errno {
	panic("implement me")
}

func (m *kvMeta) Rename(ctx *Context, parentSrc Ino, nameSrc string, parentDst Ino, nameDst string, flags uint32, inode *Ino, attr *Attr) syscall.Errno {
	panic("implement me")
}

func (m *kvMeta) Link(ctx *Context, inodeSrc, parent Ino, name string, attr *Attr) syscall.Errno {
	panic("implement me")
}

func (m *kvMeta) Readdir(ctx *Context, inode Ino, entries *[]*Entry) syscall.Errno {
	*entries = []*Entry{}
	dirInodeItem := &inodeItem{}
	buf, err := m.get(m.inodeKey(inode))
	if err != nil {
		return syscall.EIO
	}
	if buf == nil {
		return syscall.ENOENT
	}
	m.parseInode(buf, dirInodeItem)
	parentIno := dirInodeItem.parentIno

	entry, err := m.get(m.entryKey(parentIno, string(dirInodeItem.name)))

	if entry != nil {
		dirEntryItem := &entryItem{}
		m.parseEntry(entry, dirEntryItem)
		if dirEntryItem.done == entryDone {
			ens, err := m.scanValues(m.entryKey(inode, ""))
			if err != nil {
				return utils.ToSyscallErrno(err)
			}
			prefix := len(m.entryKey(inode, ""))
			var childEntryItem *entryItem
			for name, childEntryBuf := range ens {
				childEntryItem = &entryItem{}
				m.parseEntry(childEntryBuf, childEntryItem)
				en := &Entry{
					Ino:  childEntryItem.ino,
					Name: name[prefix:],
					Attr: &Attr{Mode: childEntryItem.mode},
				}
				*entries = append(*entries, en)
			}
			return syscall.F_OK
		}
	}
	absolutePath := m.fullPath(inode)
	ufs, isLink, _, path := m.GetUFS(absolutePath)

	err = m.txn(func(tx kv_new.KvTxn) error {
		dirs, err := ufs.ReadDir(path)
		if err != nil {
			return utils.ToSyscallErrno(err)
		}
		now := time.Now()

		// 不存在link嵌套，因此只有非link情况下才需要填充link文件
		if !isLink {
			m.ufsMap.Range(func(key, value interface{}) bool {
				dir := pathlib.Dir(key.(string))
				linkUfs := value.(ufslib.UnderFileStorage)
				if pathlib.Clean("/"+absolutePath) == pathlib.Clean("/"+dir) {
					entry := ufslib.DirEntry{
						Name: pathlib.Base(key.(string)),
					}
					attr, err := linkUfs.GetAttr("")
					if err != nil {
						// TODO: 获取link文件报错，暂不往上抛出
						log.Errorf("getAttr : name[%s] failed: [%v]", key.(string), err)
					}
					entry.Attr.Mode = uint32(os.ModeSymlink | attr.Mode)
					// 如果原位置已有文件，默认展示Link文件覆盖原来的文件，unlink后可见
					exist := false
					for i, dirEntry := range dirs {
						log.Debugf("dirEntry.Name[%s] and key[%s]", dirEntry.Name, key.(string))
						if pathlib.Clean(dirEntry.Name) == pathlib.Clean(pathlib.Base(key.(string))) {
							exist = true
							dirs[i] = entry
							break
						}
					}
					if !exist {
						dirs = append(dirs, entry)
					}
				}
				return true
			})
		}
		log.Debugf("dirs is [%+v]", dirs)

		var childEntryItem *Entry
		var expire int64
		var childEntryBuf []byte
		var insertChildInode *inodeItem
		for _, dir := range dirs {
			childEntryItem = &Entry{
				Name: dir.Name,
				Attr: &Attr{
					Type:      dir.Attr.Type,
					Mode:      dir.Attr.Mode,
					Uid:       dir.Attr.Uid,
					Gid:       dir.Attr.Gid,
					Rdev:      dir.Attr.Rdev,
					Atime:     dir.Attr.Atime,
					Mtime:     dir.Attr.Mtime,
					Ctime:     dir.Attr.Ctime,
					Atimensec: dir.Attr.Atimensec,
					Mtimensec: dir.Attr.Mtimensec,
					Ctimensec: dir.Attr.Ctimensec,
					Nlink:     dir.Attr.Nlink,
					Size:      dir.Attr.Size,
					Blksize:   dir.Attr.Blksize,
					Block:     dir.Attr.Block,
				},
			}
			var newInode Ino

			childEntryBuf, err = m.get(m.entryKey(inode, dir.Name))
			if err != nil {
				return err
			}
			var childEntryItemFromCache *entryItem
			if childEntryBuf != nil {
				childEntryItemFromCache = &entryItem{}
				m.parseEntry(childEntryBuf, childEntryItemFromCache)
				newInode = childEntryItemFromCache.ino
			} else {
				newInodeNumber, err := m.client.NextNumber([]byte(nextInodeKey))
				if err != nil {
					return err
				}
				newInode = Ino(newInodeNumber)
				insertChildEntry := &entryItem{
					ino:  newInode,
					mode: dir.Attr.Mode,
				}
				err = tx.Set(m.entryKey(inode, dir.Name), m.marshalEntry(insertChildEntry))
				if err != nil {
					return err
				}
			}
			expire = now.Add(m.attrTimeOut).Unix()
			insertChildInode = &inodeItem{
				attr:      *childEntryItem.Attr,
				parentIno: inode,
				expire:    expire,
				name:      []byte(dir.Name),
			}
			err = tx.Set(m.inodeKey(newInode), m.marshalInode(insertChildInode))
			if err != nil {
				return err
			}
			childEntryItem.Ino = newInode
			*entries = append(*entries, childEntryItem)
		}
		insertDirEntry := &entryItem{
			ino:    inode,
			mode:   dirInodeItem.attr.Mode,
			expire: now.Add(m.entryTimeOut).Unix(),
			done:   entryDone,
		}
		err = tx.Set(m.entryKey(parentIno, string(dirInodeItem.name)), m.marshalEntry(insertDirEntry))
		return err
	})
	if err != nil {
		return utils.ToSyscallErrno(err)
	}
	return syscall.F_OK
}

func (m *kvMeta) Create(ctx *Context, parent Ino, name string, mode uint32, cumask uint16, flags uint32, inode *Ino, attr *Attr) (ufslib.UnderFileStorage, string, syscall.Errno) {
	panic("implement me")
}

func (m *kvMeta) Open(ctx *Context, inode Ino, flags uint32, attr *Attr) (ufslib.UnderFileStorage, string, syscall.Errno) {
	panic("implement me")
}

func (m *kvMeta) Close(ctx *Context, inode Ino) syscall.Errno {
	panic("implement me")
}

func (m *kvMeta) Read(ctx *Context, inode Ino, indx uint32, buf []byte) syscall.Errno {
	panic("implement me")
}

func (m *kvMeta) Write(ctx *Context, inode Ino, off uint32, length int) syscall.Errno {
	panic("implement me")
}

func (m *kvMeta) CopyFileRange(ctx *Context, fin Ino, offIn uint64, fout Ino, offOut uint64, size uint64, flags uint32, copied *uint64) syscall.Errno {
	panic("implement me")
}

func (m *kvMeta) GetXattr(ctx *Context, inode Ino, attribute string, vbuff *[]byte) syscall.Errno {
	panic("implement me")
}

func (m *kvMeta) ListXattr(ctx *Context, inode Ino, dbuff *[]string) syscall.Errno {
	panic("implement me")
}

func (m *kvMeta) SetXattr(ctx *Context, inode Ino, name string, value []byte, flags uint32) syscall.Errno {
	panic("implement me")
}

func (m *kvMeta) RemoveXattr(ctx *Context, inode Ino, name string) syscall.Errno {
	panic("implement me")
}

func (m *kvMeta) Flock(ctx *Context, inode Ino, owner uint64, ltype uint32, block bool) syscall.Errno {
	panic("implement me")
}

func (m *kvMeta) Getlk(ctx *Context, inode Ino, owner uint64, ltype *uint32, start, end *uint64, pid *uint32) syscall.Errno {
	panic("implement me")
}

func (m *kvMeta) Setlk(ctx *Context, inode Ino, owner uint64, block bool, ltype uint32, start, end uint64, pid uint32) syscall.Errno {
	panic("implement me")
}

func (m *kvMeta) DumpMeta(w io.Writer) error {
	panic("implement me")
}

func (m *kvMeta) LoadMeta(r io.Reader) error {
	panic("implement me")
}

func (m *kvMeta) LinksMetaUpdateHandler(stopChan chan struct{}, interval int, linkMetaDirPrefix string) error {
	panic("implement me")
}

func (m *kvMeta) InoToPath(inode Ino) string {
	panic("implement me")
}
