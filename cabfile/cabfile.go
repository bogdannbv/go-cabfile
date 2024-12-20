// Copyright 2019 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package cabfile provides a bare minimum implementation of a parser for the
// Microsoft Cabinet file format. Its goal is to support the feature set of
// Cabinet files produced by gcab for the LVFS project.
//
// Normative references for this implementation are [MS-CAB] for the Cabinet
// file format and [MS-MCI] for the Microsoft ZIP Compression and Decompression
// Data Structure.
//
// [MS-CAB]: http://download.microsoft.com/download/4/d/a/4da14f27-b4ef-4170-a6e6-5b1ef85b1baa/[ms-cab].pdf
// [MS-MCI]: http://interoperability.blob.core.windows.net/files/MS-MCI/[MS-MCI].pdf
package cabfile

import (
	"bufio"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Cabinet provides read-only access to Microsoft Cabinet files.
type Cabinet struct {
	r     io.ReadSeeker
	hdr   *Header
	fldrs []*folder
	files []*file
}

type Header struct {
	Signature    [4]byte
	Reserved1    uint32
	CBCabinet    uint32 // size of this cabinet file in bytes
	Reserved2    uint32
	COFFFiles    uint32 // offset of the first CFFILE entry
	Reserved3    uint32 // reserved
	VersionMinor uint8  // cabinet file format version, minor
	VersionMajor uint8  // cabinet file format version, major
	CFolders     uint16 // number of CFFOLDER entries in this cabinet
	CFiles       uint16 // number of CFFILE entries in this cabinet
	Flags        uint16 // cabinet file option indicators
	SetID        uint16 // must be the same for all cabinets in a set
	ICabinet     uint16 // number of this cabinet file in a set
	CBCFHeader   uint16 // size of abReserve field in the CFHeader in bytes (optional)
	CBCFFolder   uint8  // size of abReserve field in each CFFolder entry in bytes (optional)
	CBCFData     uint8  // size of abReserve field in each CFData entry in bytes (optional)
}

const (
	hdrPrevCabinet uint16 = 1 << iota
	hdrNextCabinet
	hdrReservePresent
)

type folder struct {
	COFFCabStart uint32 // offset of the first CFDATA block in this folder
	CCFData      uint16 // number of CFDATA blocks in this folder
	TypeCompress uint16 // compression type indicator
}

const (
	compMask    uint16 = 0xf
	compNone           = 0x0
	compMSZIP          = 0x1
	compQuantum        = 0x2
	compLZX            = 0x3
)

type cfFile struct {
	CBFile          uint32 // uncompressed size of this file in bytes
	UOffFolderStart uint32 // uncompressed offset of this file in the folder
	IFolder         uint16 // index into the CFFOLDER area
	Date            uint16 // date stamp for this file
	Time            uint16 // time stamp for this file
	Attribs         uint16 // attribute flags for this file
}

const (
	attribReadOnly = 1 << iota // file is read-only
	attribHidden               // file is hidden
	attribSystem               // file is a system file
	_
	_
	attribArchive   // file modified since last backup
	attribExec      // run after extraction
	attribNameIsUTF // filename is UTF-encoded
)

type file struct {
	*cfFile
	name string
}

type cfData struct {
	Checksum uint32 // checksum of this CFDATA entry
	CBData   uint16 // number of compressed bytes in this block
	CBUncomp uint16 // number of uncompressed bytes in this block
}

// New returns a new Cabinet with the header structures parsed and sanity checked.
func New(r io.ReadSeeker) (*Cabinet, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("could not seek to the beginning: %v", err)
	}

	// CFHEADER
	var hdr Header
	if err := binary.Read(r, binary.LittleEndian, &hdr.Signature); err != nil {
		return nil, fmt.Errorf("could not deserialize header signature: %w", err)
	}
	if err := binary.Read(r, binary.LittleEndian, &hdr.Reserved1); err != nil {
		return nil, fmt.Errorf("could not deserialize header reserved1: %w", err)
	}
	if err := binary.Read(r, binary.LittleEndian, &hdr.CBCabinet); err != nil {
		return nil, fmt.Errorf("could not deserialize header cbCabinet: %w", err)
	}
	if err := binary.Read(r, binary.LittleEndian, &hdr.Reserved2); err != nil {
		return nil, fmt.Errorf("could not deserialize header reserved2: %w", err)
	}
	if err := binary.Read(r, binary.LittleEndian, &hdr.COFFFiles); err != nil {
		return nil, fmt.Errorf("could not deserialize header coffFiles: %w", err)
	}
	if err := binary.Read(r, binary.LittleEndian, &hdr.Reserved3); err != nil {
		return nil, fmt.Errorf("could not deserialize header reserved3: %w", err)
	}
	if err := binary.Read(r, binary.LittleEndian, &hdr.VersionMinor); err != nil {
		return nil, fmt.Errorf("could not deserialize header versionMinor: %w", err)
	}
	if err := binary.Read(r, binary.LittleEndian, &hdr.VersionMajor); err != nil {
		return nil, fmt.Errorf("could not deserialize header versionMajor: %w", err)
	}
	if err := binary.Read(r, binary.LittleEndian, &hdr.CFolders); err != nil {
		return nil, fmt.Errorf("could not deserialize header cFolder: %w", err)
	}
	if err := binary.Read(r, binary.LittleEndian, &hdr.CFiles); err != nil {
		return nil, fmt.Errorf("could not deserialize header cFiles: %w", err)
	}
	if err := binary.Read(r, binary.LittleEndian, &hdr.Flags); err != nil {
		return nil, fmt.Errorf("could not deserialize header flags: %w", err)
	}
	if err := binary.Read(r, binary.LittleEndian, &hdr.SetID); err != nil {
		return nil, fmt.Errorf("could not deserialize header setID: %w", err)
	}
	if err := binary.Read(r, binary.LittleEndian, &hdr.ICabinet); err != nil {
		return nil, fmt.Errorf("could not deserialize header iCabinet: %w", err)
	}

	// if cfhdrRESERVE_PRESENT flag is set, parse the optional abReserve size fields
	if (hdr.Flags & hdrReservePresent) != 0 {
		if err := binary.Read(r, binary.LittleEndian, &hdr.CBCFHeader); err != nil {
			return nil, fmt.Errorf("could not deserialize header cbCFHeader: %w", err)
		}
		if err := binary.Read(r, binary.LittleEndian, &hdr.CBCFFolder); err != nil {
			return nil, fmt.Errorf("could not deserialize header cbCFFolder: %w", err)
		}
		if err := binary.Read(r, binary.LittleEndian, &hdr.CBCFData); err != nil {
			return nil, fmt.Errorf("could not deserialize header cbCFData: %w", err)
		}
	}

	if !bytes.Equal(hdr.Signature[:], []byte("MSCF")) {
		return nil, fmt.Errorf("invalid Cabinet file signature: %v", hdr.Signature)
	}
	if hdr.Reserved1 != 0 || hdr.Reserved2 != 0 || hdr.Reserved3 != 0 {
		return nil, fmt.Errorf("reserved files must be zero: %v, %v, %v", hdr.Reserved1, hdr.Reserved2, hdr.Reserved3)
	}
	if hdr.VersionMajor != 1 || hdr.VersionMinor != 3 {
		return nil, fmt.Errorf("Cabinet file version has unsupported version %d.%d", hdr.VersionMajor, hdr.VersionMinor)
	}
	if (hdr.Flags&hdrPrevCabinet) != 0 || (hdr.Flags&hdrNextCabinet) != 0 {
		return nil, errors.New("multi-part Cabinet files are unsupported")
	}

	// skip abReserve by reading cbCFHeader bytes, discarding the result
	if _, err := io.ReadFull(r, make([]byte, hdr.CBCFHeader)); err != nil {
		return nil, fmt.Errorf("could not skip %d header abReserve bytes: %w", hdr.CBCFHeader, err)
	}

	// CFFOLDER
	var fldrs []*folder
	for i := uint16(0); i < hdr.CFolders; i++ {
		var fldr folder
		if err := binary.Read(r, binary.LittleEndian, &fldr); err != nil {
			return nil, fmt.Errorf("could not deserialize folder %d: %v", i, err)
		}
		switch fldr.TypeCompress & compMask {
		case compNone:
		case compMSZIP:
		default:
			return nil, fmt.Errorf("folder compressed with unsupported algorithm %d", fldr.TypeCompress)
		}
		fldrs = append(fldrs, &fldr)
	}

	// CFFILE
	if _, err := r.Seek(int64(hdr.COFFFiles), io.SeekStart); err != nil {
		return nil, fmt.Errorf("could not seek to start of CFFILE section: %v", err)
	}
	var files []*file
	for i := uint16(0); i < hdr.CFiles; i++ {
		var f cfFile
		if err := binary.Read(r, binary.LittleEndian, &f); err != nil {
			return nil, fmt.Errorf("could not deserialize file %d: %v", i, err)
		}
		off, err := r.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, fmt.Errorf("could not preserve current offset: %v", err)
		}
		fn, err := bufio.NewReader(r).ReadBytes('\x00')
		if err != nil {
			return nil, fmt.Errorf("could not read filename for file %d: %v", i, err)
		}
		if _, err := r.Seek(off+int64(len(fn)), io.SeekStart); err != nil {
			return nil, fmt.Errorf("could not seek to the end of file entry %d: %v", i, err)
		}
		files = append(files, &file{&f, string(fn[:len(fn)-1])})
	}

	return &Cabinet{r, &hdr, fldrs, files}, nil
}

// FileList returns the list of filenames in the Cabinet file.
func (c *Cabinet) FileList() []string {
	var names []string
	for _, f := range c.files {
		names = append(names, f.name)
	}
	return names
}

func (c *Cabinet) folderData(idx uint16) (io.ReadSeeker, error) {
	if int(idx) >= len(c.fldrs) {
		return nil, errors.New("folder number out of range")
	}
	fldr := c.fldrs[idx]
	if _, err := c.r.Seek(int64(fldr.COFFCabStart), io.SeekStart); err != nil {
		return nil, fmt.Errorf("could not seek to start of data section: %v", err)
	}

	// MS-ZIP requires that the history buffer is preserved across block boundaries
	var history []byte

	var buf bytes.Buffer
	for i := uint16(0); i < fldr.CCFData; i++ {
		var d cfData
		if err := binary.Read(c.r, binary.LittleEndian, &d); err != nil {
			return nil, fmt.Errorf("could not deserialize data structure %d: %v", i, err)
		}
		block := make([]byte, d.CBData)
		if n, err := c.r.Read(block); n != int(d.CBData) {
			return nil, fmt.Errorf("invalid read of size %d in data block %d; expected %d bytes", n, i, d.CBData)
		} else if err != nil {
			return nil, fmt.Errorf("could not read data block %d: %v", i, err)
		}
		// TODO: Checksum the block
		switch fldr.TypeCompress {
		case compNone:
			if d.CBData != d.CBUncomp {
				return nil, fmt.Errorf("compressed bytes %d of data section %d do not equal uncompressed bytes %d when no compression was specified", d.CBData, i, d.CBUncomp)
			}
			buf.Write(block)
		case compMSZIP:
			if !bytes.Equal(block[:2], []byte("CK")) {
				return nil, fmt.Errorf("invalid MS-ZIP signature %q in data block %d", block[:2], i)
			}
			var r io.ReadCloser
			if len(history) == 0 {
				r = flate.NewReader(bytes.NewReader(block[2:]))
			} else {
				r = flate.NewReaderDict(bytes.NewReader(block[2:]), history)
			}
			data := make([]byte, d.CBUncomp)
			if n, err := r.Read(data); n != int(d.CBUncomp) {
				return nil, fmt.Errorf("invalid decompression of size %d in data block %d; expected %d bytes", n, i, d.CBUncomp)
			} else if err != nil && err != io.EOF {
				return nil, fmt.Errorf("could not decompress data block %d: %v", i, err)
			}
			buf.Write(data)
			history = data
		default:
			return nil, errors.New("unsupported compression")
		}
	}
	return bytes.NewReader(buf.Bytes()), nil
}

// Header returns the Cabinet file Header.
func (c *Cabinet) Header() Header {
	return *c.hdr
}

// Content returns the content of the file specified by its filename as an
// io.Reader. Note that the entire folder which contains the file in question
// is decompressed for every file request.
func (c *Cabinet) Content(name string) (io.Reader, error) {
	for _, f := range c.files {
		if f.name != name {
			continue
		}
		data, err := c.folderData(f.IFolder)
		if err != nil {
			return nil, fmt.Errorf("could not acquire uncompressed data for folder %d: %v", f.IFolder, err)
		}
		if _, err := data.Seek(int64(f.UOffFolderStart), io.SeekStart); err != nil {
			return nil, fmt.Errorf("could not seek to start of data: %v", err)
		}
		blob := make([]byte, f.CBFile)
		if n, err := data.Read(blob); n != int(f.CBFile) {
			return nil, fmt.Errorf("invalid read of size %d of file data; expected %d", n, f.CBFile)
		} else if err != nil {
			return nil, fmt.Errorf("could not read file data: %v", err)
		}
		return bytes.NewReader(blob), nil
	}
	return nil, fmt.Errorf("file %q not found in Cabinet", name)
}
