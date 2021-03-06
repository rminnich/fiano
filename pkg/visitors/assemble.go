// Copyright 2018 the LinuxBoot Authors. All rights reserved
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package visitors

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"sort"

	"github.com/linuxboot/fiano/pkg/lzma"
	"github.com/linuxboot/fiano/pkg/uefi"
)

// Assemble reconstitutes the firmware tree assuming that the leaf node buffers are accurate
type Assemble struct {
}

// Run just applies the visitor.
func (v *Assemble) Run(f uefi.Firmware) error {
	return f.Apply(v)
}

// Visit applies the Assemble visitor to any Firmware type.
func (v *Assemble) Visit(f uefi.Firmware) error {
	var err error

	// Get the damn Erase Polarity
	if f, ok := f.(*uefi.FirmwareVolume); ok {
		// Set Erase Polarity
		uefi.Attributes.ErasePolarity = f.GetErasePolarity()
	}

	// We first assemble the children.
	// Sounds horrible but has to be done =(
	if err = f.ApplyChildren(v); err != nil {
		return err
	}

	switch f := f.(type) {

	case *uefi.FirmwareVolume:
		if len(f.Files) == 0 {
			// No children, buffer should already contain data.
			return nil
		}
		// We assume the buffer already contains the header. We repopulate the header from the buffer
		// Construct the full buffer.
		// The FV header is the only thing we've read in so far.
		fBuf := f.Buf()
		fBufLen := uint64(len(fBuf))
		// The reason I check against f.Length and fBuf instead of the min size is that the volume could
		// have extended headers.
		if f.Length < fBufLen {
			return fmt.Errorf("buffer read in bigger than FV length!, expected %v got %v bytes",
				f.Length, fBufLen)
		}

		fileOffset := f.DataOffset
		if f.DataOffset != fBufLen {
			return fmt.Errorf("fv header buffer size mismatch with DataOffset! buflen was %#x, DataOffset was %#x",
				fBufLen, f.DataOffset)
		}

		for _, file := range f.Files {
			fileBuf := file.Buf()
			fileLen := uint64(len(fileBuf))
			if fileLen == 0 {
				log.Fatal(file.Header.UUID)
			}

			// Pad to the 8 byte alignments.
			alignedOffset := uefi.Align8(fileOffset)
			// Read out the file alignment requirements
			if alignBase := file.Header.Attributes.GetAlignment(); alignBase != 1 {
				hl := file.HeaderLen()
				// We need to align the data, not the header. This is so terrible.
				fileDataOffset := uefi.Align(alignedOffset+hl, alignBase)
				// Calculate the starting offset of the file
				newOffset := fileDataOffset - hl
				if gap := (newOffset - alignedOffset); gap >= 8 && gap < uefi.FileHeaderMinLength {
					// We need to re align to the next boundary cause we can't put a pad file in here.
					// Who thought this was a good idea?
					fileDataOffset = uefi.Align(fileDataOffset+1, alignBase)
					newOffset = fileDataOffset - hl
				}
				if newOffset != alignedOffset {
					// Add a pad file starting from alignedOffset to newOffset
					pfile, err := uefi.CreatePadFile(newOffset - alignedOffset)
					if err != nil {
						return err
					}
					if err = f.InsertFile(alignedOffset, pfile.Buf()); err != nil {
						return fmt.Errorf("File %s: %v", pfile.Header.UUID, err)
					}
				}
				alignedOffset = newOffset
			}
			if err = f.InsertFile(alignedOffset, fileBuf); err != nil {
				return fmt.Errorf("File %s: %v", file.Header.UUID, err)
			}
			fileOffset = alignedOffset + fileLen
		}

		newFVLen := uint64(len(f.Buf()))
		if f.Length < newFVLen {
			// We've expanded the FV, resize
			if f.Blocks[0].Size == 0 {
				return fmt.Errorf("first block in FV has zero size! block was %v", f.Blocks[0])
			}
			// Align to the next block boundary
			// Make sure there are enough blocks for the length
			f.Length = uefi.Align(newFVLen, uint64(f.Blocks[0].Size))
			// Right now we assume there's only one block entry
			// TODO: handle multiple block entries
			f.Blocks[0].Count = uint32(f.Length / uint64(f.Blocks[0].Size))
		}
		if f.Length > newFVLen {
			// If the buffer is not long enough, pad ErasePolarity
			extLen := f.Length - newFVLen
			emptyBuf := make([]byte, extLen)
			uefi.Erase(emptyBuf, uefi.Attributes.ErasePolarity)
			f.SetBuf(append(f.Buf(), emptyBuf...))
		}

		fBuf = f.Buf()

		// Write the length to the correct spot
		// TODO: handle the whole header instead of doing this
		binary.LittleEndian.PutUint64(fBuf[32:], f.Length)

		// Write the block map count
		binary.LittleEndian.PutUint32(fBuf[56:], f.Blocks[0].Count)
		// Checksum the header again
		// TODO: handle the whole header instead of doing this
		// First we zero out the original checksum
		binary.LittleEndian.PutUint16(fBuf[50:], 0)
		sum, err := uefi.Checksum16(fBuf[:f.HeaderLen])
		if err != nil {
			return err
		}
		newSum := 0 - sum
		binary.LittleEndian.PutUint16(fBuf[50:], newSum)

		// Save the buffer
		f.SetBuf(fBuf)

	case *uefi.File:
		fh := &f.Header
		var fBuf []byte
		if len(f.Sections) == 0 {
			// No children, buffer should already contain data.
			// we don't support this file type, just return the raw buffer.
			// Or we've removed the sections and just want to replace the file directly
			// We have to make sure the state is correct, so we still need to write out
			// the file header.

			// Set state to valid based on erase polarity
			// We really should redo the whole header
			// TODO: Reconstruct header from JSON
			fh.State = 0x07 ^ uefi.Attributes.ErasePolarity
			fBuf = f.Buf()
			fBuf[0x17] = fh.State
			f.SetBuf(fBuf)
			return nil
		}

		// Otherwise, we reconstruct the entire file from the sections and the
		// file header using data from the JSON. This means that some JSON values
		// are now respected, including GUID changes. However file lengths and
		// checksums will be recalculated.

		// Assemble all sections so we know the final file size. We need to do this
		// to know if we need to use the extended header.
		fileData := []byte{}
		dLen := uint64(0)
		for _, s := range f.Sections {
			// Align to 4 bytes and extend with 00s
			// Why is it 00s? I don't know. Everything else has been extended with FFs
			// but somehow in between sections alignment is done with 0s. What the heck.
			for count := uefi.Align4(dLen) - dLen; count > 0; count-- {
				fileData = append(fileData, 0x00)
			}
			dLen = uefi.Align4(dLen)

			// Append the section
			sData := s.Buf()
			dLen += uint64(len(sData))
			fileData = append(fileData, sData...)
		}

		f.SetSize(uefi.FileHeaderMinLength+dLen, true)

		// Set state to valid based on erase polarity
		fh.State = 0x07 ^ uefi.Attributes.ErasePolarity

		if err = f.ChecksumAndAssemble(fileData); err != nil {
			return err
		}
		return nil

	case *uefi.Section:
		if len(f.Encapsulated) == 0 {
			// No children, buffer should already contain data.
			return nil
		}

		// Construct the section data
		secData := []byte{}
		dLen := uint64(0)
		for _, es := range f.Encapsulated {
			// Align to 4 bytes and extend with 00s
			for count := uefi.Align4(dLen) - dLen; count > 0; count-- {
				secData = append(secData, 0x00)
			}
			dLen = uefi.Align4(dLen)

			esData := es.Value.Buf()
			dLen += uint64(len(esData))
			secData = append(secData, esData...)
		}

		// Special processing for some section types
		switch f.Header.Type {
		case uefi.SectionTypeGUIDDefined:
			ts := f.TypeSpecific.Header.(*uefi.SectionGUIDDefined)
			if ts.Attributes&uint16(uefi.GUIDEDSectionProcessingRequired) != 0 {
				var fBuf []byte
				switch ts.GUID {
				case uefi.LZMAGUID:
					fBuf, err = lzma.Encode(secData)
					f.SetBuf(fBuf)
					if err != nil {
						return err
					}
				case uefi.LZMAX86GUID:
					fBuf, err = lzma.EncodeX86(secData)
					if err != nil {
						return err
					}
				default:
					return fmt.Errorf("unknown guid defined from section %v, should not have encapsulated sections", f)
				}
				f.SetBuf(fBuf)
			}
		default:
			f.SetBuf(secData)
		}

		// Fix up the header
		err = f.GenSecHeader()

	case *uefi.FlashDescriptor:
		err = f.ParseFlashDescriptor()

	case *uefi.BIOSRegion:
		fBuf := make([]byte, f.Length)
		firstFV, err := f.FirstFV()
		if err != nil {
			return err
		}
		uefi.Attributes.ErasePolarity = firstFV.GetErasePolarity()
		uefi.Erase(fBuf, uefi.Attributes.ErasePolarity)
		// Put the elements together
		offset := uint64(0)
		for _, e := range f.Elements {
			// copy the fv over the original
			// TODO: handle different sizes.
			// We'll have to FF out the new regions/ check for clashes
			ebuf := e.Value.Buf()
			copy(fBuf[offset:offset+uint64(len(ebuf))], ebuf)
			offset += uint64(len(ebuf))
		}
		// Set the buffer
		f.SetBuf(fBuf)

		return nil

	case *uefi.FlashImage:
		ifdbuf := f.IFD.Buf()
		// Assemble regions.
		// We need to sort them since a) we don't really know the order until we parse the block numbers
		// and b) the order may have changed anyway.
		if !f.IFD.Region.BIOS.Valid() {
			return fmt.Errorf("no BIOS region: invalid region parameters %v", f.IFD.Region.BIOS)
		}
		type region struct {
			P   *uefi.Region
			buf []byte
		}
		regions := make([]region, 0, 4)

		if f.BIOS == nil {
			return errors.New("bios struct is nil, firmware tree is malformed")
		}
		// Point position to struct read from IFD rather than json.
		f.BIOS.Position = &f.IFD.Region.BIOS
		biosbuf := f.BIOS.Buf()
		regions = append(regions, region{f.BIOS.Position, biosbuf})

		// ME region
		if f.IFD.Region.ME.Valid() {
			if f.ME == nil {
				// Not in tree, error out since we don't have an ExtractPath.
				return errors.New("no ME region unmarshalled from JSON, but ME region is present in IFD")
			}
			f.ME.Position = &f.IFD.Region.ME
			mebuf := f.ME.Buf()
			regions = append(regions, region{f.ME.Position, mebuf})
		}

		// GBE region
		if f.IFD.Region.GBE.Valid() {
			if f.GBE == nil {
				// Not in tree, error out since we don't have an ExtractPath.
				return errors.New("no GBE region unmarshalled from JSON, but GBE region is present in IFD")
			}
			f.GBE.Position = &f.IFD.Region.GBE
			gbebuf := f.GBE.Buf()
			regions = append(regions, region{f.GBE.Position, gbebuf})
		}

		// PD region
		if f.IFD.Region.PD.Valid() {
			if f.PD == nil {
				// Not in JSON, error out since we don't have an ExtractPath.
				return errors.New("no PD region unmarshalled from JSON, but PD region is present in IFD")
			}
			f.PD.Position = &f.IFD.Region.PD
			pdbuf := f.PD.Buf()
			regions = append(regions, region{f.PD.Position, pdbuf})
		}

		// Sort regions so we can output the flash file correctly.
		sort.Slice(regions, func(i, j int) bool { return regions[i].P.Base < regions[j].P.Base })
		// append all slices together and return.
		fBuf := make([]byte, 0, 0)
		fBuf = append(fBuf, ifdbuf...)
		for _, r := range regions {
			fBuf = append(fBuf, r.buf...)
		}

		f.SetBuf(fBuf)
		return nil

	}

	return err

}
