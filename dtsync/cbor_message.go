// Code adapted from original generated by github.com/whyrusleeping/cbor-gen.
// This adapted code allows for an optional OrigPeer field.
//
// TODO: Convert Message into IPLD schema and use bindnode for serialization.

package dtsync

import (
	"fmt"
	"io"

	cbg "github.com/whyrusleeping/cbor-gen"
)

func (t *Message) MarshalCBOR(w io.Writer) error {
	var err error

	if t == nil {
		_, err = w.Write(cbg.CborNull)
		return err
	}

	var lengthBufMessage []byte
	if t.OrigPeer == "" {
		lengthBufMessage = []byte{131}
	} else {
		lengthBufMessage = []byte{132}
	}
	if _, err = w.Write(lengthBufMessage); err != nil {
		return err
	}

	scratch := make([]byte, 9)

	// Encode t.Cid.
	if err = cbg.WriteCidBuf(scratch, w, t.Cid); err != nil {
		return fmt.Errorf("failed to write cid field t.Cid: %w", err)
	}

	// Encode t.Addrs.
	if len(t.Addrs) > cbg.MaxLength {
		return fmt.Errorf("slice value in field t.Addrs was too long")
	}

	if err = cbg.WriteMajorTypeHeaderBuf(scratch, w, cbg.MajArray, uint64(len(t.Addrs))); err != nil {
		return err
	}
	for _, v := range t.Addrs {
		if len(v) > cbg.ByteArrayMaxLen {
			return fmt.Errorf("byte array in field v was too long")
		}

		if err = cbg.WriteMajorTypeHeaderBuf(scratch, w, cbg.MajByteString, uint64(len(v))); err != nil {
			return err
		}

		if _, err = w.Write(v[:]); err != nil {
			return err
		}
	}

	if len(t.ExtraData) > cbg.ByteArrayMaxLen {
		return fmt.Errorf("byte array in field t.ExtraData was too long")
	}

	if err = cbg.WriteMajorTypeHeaderBuf(scratch, w, cbg.MajByteString, uint64(len(t.ExtraData))); err != nil {
		return err
	}

	if _, err = w.Write(t.ExtraData[:]); err != nil {
		return err
	}

	// OrigPeer is empty so do not encode it.
	if len(t.OrigPeer) == 0 {
		return nil
	}

	// Encode t.OrigPeer.
	if len(t.OrigPeer) > cbg.MaxLength {
		return fmt.Errorf("value in field t.OrigPeer was too long")
	}

	if err = cbg.WriteMajorTypeHeaderBuf(scratch, w, cbg.MajTextString, uint64(len(t.OrigPeer))); err != nil {
		return err
	}
	if _, err = io.WriteString(w, string(t.OrigPeer)); err != nil {
		return err
	}

	return nil
}

func (t *Message) UnmarshalCBOR(r io.Reader) error {
	*t = Message{}

	br := cbg.GetPeeker(r)
	scratch := make([]byte, 8)

	maj, extra, err := cbg.CborReadHeaderBuf(br, scratch)
	if err != nil {
		return err
	}
	if maj != cbg.MajArray {
		return fmt.Errorf("cbor input should be of type array")
	}

	if extra > 4 {
		return fmt.Errorf("cbor input had too many fields")
	}
	if extra < 3 {
		return fmt.Errorf("cbor input had too few fields")
	}
	var hasOrigPeer bool
	if extra == 4 {
		hasOrigPeer = true
	}

	// Decode t.Cid.
	t.Cid, err = cbg.ReadCid(br)
	if err != nil {
		return fmt.Errorf("failed to read cid field t.Cid: %w", err)
	}

	// Decode t.Addrs.
	maj, extra, err = cbg.CborReadHeaderBuf(br, scratch)
	if err != nil {
		return err
	}

	if extra > cbg.MaxLength {
		return fmt.Errorf("t.Addrs: array too large (%d)", extra)
	}

	if maj != cbg.MajArray {
		return fmt.Errorf("expected cbor array")
	}

	if extra > 0 {
		t.Addrs = make([][]uint8, extra)
	}

	for i := 0; i < int(extra); i++ {
		maj, extra, err := cbg.CborReadHeaderBuf(br, scratch)
		if err != nil {
			return err
		}

		if extra > cbg.ByteArrayMaxLen {
			return fmt.Errorf("byte array too large (%d) for Addrs[%d]", extra, i)
		}
		if maj != cbg.MajByteString {
			return fmt.Errorf("expected byte array")
		}

		if extra > 0 {
			t.Addrs[i] = make([]uint8, extra)
		}

		if _, err = io.ReadFull(br, t.Addrs[i][:]); err != nil {
			return err
		}
	}

	// Decode t.ExtraData.
	maj, extra, err = cbg.CborReadHeaderBuf(br, scratch)
	if err != nil {
		return err
	}

	if extra > cbg.ByteArrayMaxLen {
		return fmt.Errorf("byte array too large (%d) for ExtraData", extra)
	}
	if maj != cbg.MajByteString {
		return fmt.Errorf("expected byte array")
	}

	if extra > 0 {
		t.ExtraData = make([]uint8, extra)
	}

	if _, err = io.ReadFull(br, t.ExtraData[:]); err != nil {
		return err
	}

	// OrigPeer field does not exist, so nothing more to do.
	if !hasOrigPeer {
		return nil
	}

	// Decode t.OrigPeer.
	sval, err := cbg.ReadString(br)
	if err != nil {
		return err
	}
	t.OrigPeer = string(sval)

	return nil
}
