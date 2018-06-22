package uefi

import (
	"bytes"
	"encoding/binary"
	"fmt"

	uuid "github.com/linuxboot/fiano/uuid"
)

// FirmwareVolume constants
const (
	FirmwareVolumeFixedHeaderSize  = 56
	FirmwareVolumeMinSize          = FirmwareVolumeFixedHeaderSize + 8 // +8 for the null block that terminates the block list
	FirmwareVolumeExtHeaderMinSize = 20
)

// FirmwareVolumeGUIDs maps the known FV GUIDs. These values come from
// uefi-firmware-parser
var FirmwareVolumeGUIDs = map[string]string{
	"7a9354d9-0468-444a-81ce-0bf617d890df": "FFS1",
	"8c8ce578-8a3d-4f1c-9935-896185c32dd3": "FFS2",
	"5473c07a-3dcb-4dca-bd6f-1e9689e7349a": "FFS3",
	"fff12b8d-7696-4c8b-a985-2747075b4f50": "NVRAM_EVSA",
	"cef5b9a3-476d-497f-9fdc-e98143e0422c": "NVRAM_NVAR",
	"00504624-8a59-4eeb-bd0f-6b36e96128e0": "NVRAM_EVSA2",
	"04adeead-61ff-4d31-b6ba-64f8bf901f5a": "APPLE_BOOT",
	"16b45da2-7d70-4aea-a58d-760e9ecb841d": "PFH1",
	"e360bdba-c3ce-46be-8f37-b231e5cb9f35": "PFH2",
}

// Block describes number and size of the firmware volume blocks
type Block struct {
	Count uint32
	Size  uint32
}

// FirmwareVolumeFixedHeader contains the fixed fields of a firmware volume
// header
type FirmwareVolumeFixedHeader struct {
	_               [16]uint8
	FileSystemGUID  uuid.UUID
	Length          uint64
	Signature       uint32
	Attributes      uint32 // UEFI PI spec volume 3.2.1 EFI_FIRMWARE_VOLUME_HEADER
	HeaderLen       uint16
	Checksum        uint16
	ExtHeaderOffset uint16
	Reserved        uint8 `json:"-"`
	Revision        uint8
	// _               [3]uint8
}

// FirmwareVolumeExtHeader contains the fields of an extended firmware volume
// header
type FirmwareVolumeExtHeader struct {
	FVName        uuid.UUID
	ExtHeaderSize uint32
}

// FirmwareVolume represents a firmware volume. It combines the fixed header and
// a variable list of blocks
type FirmwareVolume struct {
	FirmwareVolumeFixedHeader
	// there must be at least one that is zeroed and indicates the end of the
	// block list
	// We don't really have to care about blocks because we just read everything in.
	Blocks []Block
	FirmwareVolumeExtHeader

	// Variables not in the binary for us to keep track of stuff/print
	guidString string
	guidName   string
	buf        []byte
}

// FindFirmwareVolumeOffset searches for a firmware volume signature, "_FVH"
// using 8-byte alignment. If found, returns the offset from the start of the
// bios region, otherwise returns -1.
func FindFirmwareVolumeOffset(data []byte) int64 {
	if len(data) < 32 {
		return -1
	}
	var (
		offset int64
		fvSig  = []byte("_FVH")
	)
	for offset = 32; offset < int64(len(data)); offset += 8 {
		if bytes.Equal(data[offset:offset+4], fvSig) {
			return offset - 40 // the actual volume starts 40 bytes before the signature
		}
	}
	return -1
}

// NewFirmwareVolume parses a sequence of bytes and returns a FirmwareVolume
// object, if a valid one is passed, or an error
func NewFirmwareVolume(data []byte) (*FirmwareVolume, error) {
	var fv FirmwareVolume

	if len(data) < FirmwareVolumeMinSize {
		return nil, fmt.Errorf("Firmware Volume size too small: expected %v bytes, got %v",
			FirmwareVolumeMinSize,
			len(data),
		)
	}
	reader := bytes.NewReader(data)
	if err := binary.Read(reader, binary.LittleEndian, &fv.FirmwareVolumeFixedHeader); err != nil {
		return nil, err
	}
	// read the block map
	blocks := make([]Block, 0)
	for {
		var block Block
		if err := binary.Read(reader, binary.LittleEndian, &block); err != nil {
			return nil, err
		}
		if block.Count == 0 && block.Size == 0 {
			// found the terminating block
			break
		}
		blocks = append(blocks, block)
	}
	fv.Blocks = blocks

	// Parse the extended header.
	if fv.ExtHeaderOffset != 0 && uint64(fv.ExtHeaderOffset) < fv.Length-FirmwareVolumeExtHeaderMinSize {
		// jump to ext header offset.
		r := bytes.NewReader(data[fv.ExtHeaderOffset:])
		if err := binary.Read(r, binary.LittleEndian, &fv.FirmwareVolumeExtHeader); err != nil {
			return nil, fmt.Errorf("unable to parse FV extended header, got: %v", err)
		}
	}

	var ok bool
	fv.guidString = fv.FileSystemGUID.String()
	fv.guidName, ok = FirmwareVolumeGUIDs[fv.guidString]
	if !ok {
		fv.guidName = "Unknown"
	}

	// slice the buffer
	fv.buf = data[:fv.Length]
	return &fv, nil
}
