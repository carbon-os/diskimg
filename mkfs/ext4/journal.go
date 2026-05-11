package ext4

import (
	"encoding/binary"
	"io"
)

// JBD2 constants
const (
	jbd2Magic       = 0xC03B3998
	jbd2Descriptor  = 1
	jbd2Commit      = 2
	jbd2Superblock1 = 3
	jbd2Superblock2 = 4
	jbd2Revoke      = 5

	jbd2FeatIncompatAsync     = 0x0001
	jbd2FeatIncompatChecksumV2 = 0x0004
	jbd2FeatIncompatChecksumV3 = 0x0008

	jbd2FeatCompatChecksumV1 = 0x1

	jbd2ChecksumTypesCRC32C = 4
)

// writeJournal writes the JBD2 journal (journalSize blocks) to w.
// The journal consists of:
//   block 0: journal superblock (type 4)
//   blocks 1..journalSize-1: zero (empty journal — no committed transactions)
func (fs *fsLayout) writeJournal(w io.Writer, cur *uint64) error {
	jsb := fs.buildJournalSuperblock()
	if _, err := w.Write(jsb); err != nil {
		return err
	}
	*cur += blockSize

	// Remaining journal blocks — all zero.
	zeros := make([]byte, blockSize)
	for i := uint32(1); i < fs.journalSize; i++ {
		if _, err := w.Write(zeros); err != nil {
			return err
		}
		*cur += blockSize
	}
	return nil
}

// buildJournalSuperblock builds a 4096-byte block containing the JBD2
// journal superblock at offset 0.  The journal is version 2 (s_header.h_blocktype=4),
// uses crc32c checksums, and records an empty sequence (no transactions).
func (fs *fsLayout) buildJournalSuperblock() []byte {
	buf := make([]byte, blockSize)
	be := binary.BigEndian // JBD2 is big-endian

	// s_header
	be.PutUint32(buf[0:], jbd2Magic)       // h_magic
	be.PutUint32(buf[4:], jbd2Superblock2) // h_blocktype = V2
	be.PutUint32(buf[8:], 0)               // h_sequence = 0

	// journal superblock body
	be.PutUint32(buf[12:], blockSize)          // s_blocksize
	be.PutUint32(buf[16:], fs.journalSize)     // s_maxlen (total journal blocks)
	be.PutUint32(buf[20:], 1)                  // s_first (first usable log block)
	be.PutUint32(buf[24:], 1)                  // s_sequence
	be.PutUint32(buf[28:], 1)                  // s_start (log start block; 0 = clean)
	be.PutUint32(buf[32:], 0)                  // s_errno

	// feature flags
	be.PutUint32(buf[36:], jbd2FeatCompatChecksumV1) // s_feature_compat
	be.PutUint32(buf[40:], jbd2FeatIncompatChecksumV3) // s_feature_incompat
	be.PutUint32(buf[44:], 0)                           // s_feature_ro_compat

	copy(buf[48:64], fs.opts.UUID[:]) // s_uuid

	be.PutUint32(buf[64:], 1)  // s_nr_users
	be.PutUint32(buf[68:], 5)  // s_dynsuper (not used)
	be.PutUint32(buf[72:], 32) // s_max_transaction
	be.PutUint32(buf[76:], 32) // s_max_trans_data

	buf[80] = jbd2ChecksumTypesCRC32C // s_checksum_type
	// s_padding2[3], s_padding[42*4] — all zero

	// s_checksum covers entire superblock with field zeroed
	copy(buf[48:64], fs.opts.UUID[:]) // already set, just confirming
	// JBD2 checksum at offset 1020 (last 4 bytes of 1024-byte JSB struct, but
	// the struct lives in a full 4096-byte block; checksum covers first 1024B
	// with the last-4-bytes zeroed).
	// Recompute:
	csumBuf := make([]byte, 1024)
	copy(csumBuf, buf[:1024])
	binary.BigEndian.PutUint32(csumBuf[1020:], 0) // zero before hash
	csum := crc32.ChecksumIEEE(csumBuf)           // JBD2 uses CRC32 (not CRC32c) for the JSB
	binary.BigEndian.PutUint32(buf[1020:], csum)

	return buf
}