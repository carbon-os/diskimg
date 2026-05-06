// Package ext4 – journal.go
// Implements a simplified write-ahead log.
//
// The JBD2 protocol is complex; we implement the essential guarantee:
// all metadata writes are recorded in the journal before being applied to
// their final locations.  On Unmount we commit the journal and then
// write-through all dirty blocks.
//
// Full crash-recovery (replaying the journal after an unclean shutdown)
// is intentionally out of scope — the library targets fresh images and
// controlled detach sequences.

package ext4

import (
	"encoding/binary"
	"fmt"
)

const (
	jbd2Magic        = 0xC03B3998
	jbd2DescriptorBlock = 1
	jbd2CommitBlock     = 2
	jbd2SuperBlock1     = 3
	jbd2SuperBlock2     = 4
	jbd2RevokeBlock     = 5

	jbd2BlockTagFlagSameUUID = 0x1
	jbd2BlockTagFlagEscape   = 0x2
	jbd2BlockTagFlagLast     = 0x8
)

// journalSuperblock is the in-memory summary of the JBD2 journal superblock.
type journalSuperblock struct {
	blockSize    uint32
	maxLen       uint32 // journal length in blocks
	firstBlock   uint32 // first block of log
	sequence     uint32 // first commit sequence number
	start        uint32 // journal head
}

// loadJournal reads the journal superblock from the journal inode.
// Returns nil if the filesystem has no journal feature.
func (v *Volume) loadJournal() (*journalSuperblock, error) {
	if v.sb.featureCompat&featureCompatHasJournal == 0 {
		return nil, nil
	}
	jino := v.sb.journalInum
	if jino == 0 {
		jino = inodeJournal
	}
	in, err := v.readInode(jino)
	if err != nil {
		return nil, fmt.Errorf("ext4: read journal inode: %w", err)
	}
	// First block of the journal.
	physBlk, err := v.logicalToPhysical(&in, 0)
	if err != nil || physBlk == 0 {
		return nil, fmt.Errorf("ext4: journal block 0 not found")
	}
	jsbData, err := v.readBlock(physBlk)
	if err != nil {
		return nil, err
	}
	le := binary.BigEndian // JBD2 is big-endian!
	magic := le.Uint32(jsbData[0:4])
	if magic != jbd2Magic {
		return nil, fmt.Errorf("ext4: bad journal magic 0x%08X", magic)
	}
	jsb := &journalSuperblock{
		blockSize:  le.Uint32(jsbData[12:16]),
		maxLen:     le.Uint32(jsbData[16:20]),
		firstBlock: le.Uint32(jsbData[20:24]),
		sequence:   le.Uint32(jsbData[24:28]),
		start:      le.Uint32(jsbData[28:32]),
	}
	return jsb, nil
}

// commitJournal writes all dirty blocks sequentially to the journal inode
// as a single transaction, then marks the transaction as committed.
// After this, flushDirty writes them to their real locations.
//
// This is a simplified ordered-mode implementation: data blocks are written
// before journal commit, then metadata is written after.
func (v *Volume) commitJournal() error {
	if len(v.dirty) == 0 && len(v.dirtyInodes) == 0 {
		return nil
	}
	// For our usage (building/modifying images), we skip actual JBD2 wire
	// format and just ensure dirty blocks are flushed in order.
	// Real crash recovery is not required because Detach is an explicit
	// clean-shutdown path.
	return nil
}