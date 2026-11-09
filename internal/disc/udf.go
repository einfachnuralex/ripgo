package disc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"unicode/utf16"
)

// Minimal read-only UDF reader (ECMA-167 / OSTA UDF 1.02–2.60), enough to read
// the small navigation files of a DVD (VIDEO_TS/*.IFO) or Blu-ray (BDMV/*.mpls)
// directly from an optical drive or ISO image. The file system metadata is
// never encrypted, so no decryption is needed here.

const sectorSize = 2048

// UDF descriptor tag identifiers.
const (
	tagPrimaryVolume       = 1
	tagAnchorVolumePointer = 2
	tagPartition           = 5
	tagLogicalVolume       = 6
	tagTerminating         = 8
	tagFileSet             = 256
	tagFileIdentifier      = 257
	tagAllocationExtent    = 258
	tagFileEntry           = 261
	tagExtendedFileEntry   = 266
)

// maxUDFFile limits the size of files read into memory (IFO/MPLS are far smaller).
const maxUDFFile = 16 << 20

type udfFS struct {
	r          io.ReaderAt
	closer     io.Closer
	label      string
	partitions []udfPartition
	root       longAD
}

// udfPartition is one entry of the logical volume's partition map.
type udfPartition struct {
	start uint32 // absolute sector of the physical partition
	// metadata is set for a UDF 2.50 metadata partition: logical blocks are
	// mapped through the extents of the metadata file (in the physical partition).
	metadata []extent
}

type extent struct {
	length   uint32 // bytes
	location uint32 // logical block number
	part     uint16 // partition reference number
}

type longAD struct {
	length   uint32
	location uint32
	part     uint16
}

// openUDF opens the UDF file system on the device or image at name.
func openUDF(name string) (*udfFS, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	fsys, err := newUDF(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	fsys.closer = f
	return fsys, nil
}

func newUDF(r io.ReaderAt) (*udfFS, error) {
	fsys := &udfFS{r: r}

	avdp, err := fsys.findAnchor()
	if err != nil {
		return nil, err
	}
	vdsLen := le32(avdp[16:])
	vdsLoc := le32(avdp[20:])
	if err := fsys.readVolumeDescriptors(vdsLoc, vdsLen); err != nil {
		return nil, err
	}
	return fsys, nil
}

func (u *udfFS) Label() string { return u.label }

func (u *udfFS) Close() error {
	if u.closer != nil {
		return u.closer.Close()
	}
	return nil
}

// readSectors reads count sectors starting at the absolute sector. Reads are
// always sector-aligned, which raw optical devices require.
func (u *udfFS) readSectors(sector, count uint32) ([]byte, error) {
	buf := make([]byte, int(count)*sectorSize)
	n, err := u.r.ReadAt(buf, int64(sector)*sectorSize)
	if n == len(buf) {
		return buf, nil
	}
	if err == nil || err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return nil, fmt.Errorf("reading sector %d: %w", sector, err)
}

// readTag reads one sector and checks that it holds a descriptor with tag id want.
func (u *udfFS) readTag(sector uint32, want uint16) ([]byte, error) {
	b, err := u.readSectors(sector, 1)
	if err != nil {
		return nil, err
	}
	if id, ok := tagID(b); !ok || id != want {
		return nil, fmt.Errorf("sector %d: expected UDF descriptor %d", sector, want)
	}
	return b, nil
}

// tagID returns the descriptor tag identifier of b if its tag checksum is valid.
func tagID(b []byte) (uint16, bool) {
	if len(b) < 16 {
		return 0, false
	}
	var sum byte
	for i := 0; i < 16; i++ {
		if i != 4 {
			sum += b[i]
		}
	}
	if sum != b[4] {
		return 0, false
	}
	return le16(b), true
}

func (u *udfFS) findAnchor() ([]byte, error) {
	if b, err := u.readTag(256, tagAnchorVolumePointer); err == nil {
		return b, nil
	}
	// Fallback: the anchor is also recorded at the last sector and last sector - 256.
	if s, ok := u.r.(io.Seeker); ok {
		if size, err := s.Seek(0, io.SeekEnd); err == nil && size >= 512*sectorSize {
			last := uint32(size/sectorSize) - 1
			for _, sec := range []uint32{last - 256, last} {
				if b, err := u.readTag(sec, tagAnchorVolumePointer); err == nil {
					return b, nil
				}
			}
		}
	}
	return nil, errors.New("no UDF file system found (anchor volume descriptor missing)")
}

func (u *udfFS) readVolumeDescriptors(loc, length uint32) error {
	type physical struct{ number, start uint32 }
	var parts []physical
	var lvd []byte

	for i := uint32(0); i < max(length/sectorSize, 1) && i < 64; i++ {
		b, err := u.readSectors(loc+i, 1)
		if err != nil {
			return err
		}
		id, ok := tagID(b)
		if !ok {
			continue
		}
		switch id {
		case tagPrimaryVolume:
			if u.label == "" {
				u.label = dstring(b[24:56])
			}
		case tagPartition:
			parts = append(parts, physical{number: uint32(le16(b[22:])), start: le32(b[188:])})
		case tagLogicalVolume:
			lvd = b
		}
		if id == tagTerminating {
			break
		}
	}
	if lvd == nil || len(parts) == 0 {
		return errors.New("incomplete UDF volume descriptor sequence")
	}
	if label := dstring(lvd[84:212]); label != "" {
		u.label = label
	}
	if bs := le32(lvd[212:]); bs != sectorSize {
		return fmt.Errorf("unsupported UDF logical block size %d", bs)
	}
	fsd := parseLongAD(lvd[248:])

	partStart := func(number uint32) (uint32, error) {
		for _, p := range parts {
			if p.number == number {
				return p.start, nil
			}
		}
		return 0, fmt.Errorf("UDF partition %d not found", number)
	}

	nMaps := le32(lvd[268:])
	maps := lvd[440:]
	var metadataRefs []int
	var metadataFiles []uint32
	for i := uint32(0); i < nMaps && len(maps) >= 2; i++ {
		mapType, mapLen := maps[0], int(maps[1])
		if mapLen < 6 || mapLen > len(maps) {
			return errors.New("invalid UDF partition map")
		}
		m := maps[:mapLen]
		switch mapType {
		case 1:
			start, err := partStart(uint32(le16(m[4:])))
			if err != nil {
				return err
			}
			u.partitions = append(u.partitions, udfPartition{start: start})
		case 2:
			if mapLen < 64 {
				return errors.New("invalid UDF type 2 partition map")
			}
			ident := strings.TrimRight(string(m[5:28]), "\x00")
			start, err := partStart(uint32(le16(m[38:])))
			if err != nil {
				return err
			}
			switch ident {
			case "*UDF Metadata Partition":
				metadataRefs = append(metadataRefs, len(u.partitions))
				metadataFiles = append(metadataFiles, le32(m[40:]))
			case "*UDF Sparable Partition":
				// Sparing only matters for rewritable media; pressed discs read like physical.
			default:
				return fmt.Errorf("unsupported UDF partition type %q", ident)
			}
			u.partitions = append(u.partitions, udfPartition{start: start})
		default:
			return fmt.Errorf("unsupported UDF partition map type %d", mapType)
		}
		maps = maps[mapLen:]
	}

	// The metadata file lives in the physical partition the metadata map refers to.
	for i, ref := range metadataRefs {
		start := u.partitions[ref].start
		fe, err := u.readEntry(start + metadataFiles[i])
		if err != nil {
			return fmt.Errorf("reading UDF metadata file: %w", err)
		}
		exts, _, err := u.entryExtents(fe, uint16(ref))
		if err != nil {
			return fmt.Errorf("reading UDF metadata file: %w", err)
		}
		// The extents address blocks of the physical partition (see sector).
		u.partitions[ref].metadata = exts
	}

	fsdSector, err := u.sector(fsd.part, fsd.location)
	if err != nil {
		return err
	}
	b, err := u.readTag(fsdSector, tagFileSet)
	if err != nil {
		return err
	}
	u.root = parseLongAD(b[400:])
	return nil
}

// sector translates a logical block of a partition to an absolute sector.
func (u *udfFS) sector(part uint16, lbn uint32) (uint32, error) {
	if int(part) >= len(u.partitions) {
		return 0, fmt.Errorf("invalid UDF partition reference %d", part)
	}
	p := u.partitions[part]
	if p.metadata == nil {
		return p.start + lbn, nil
	}
	off := uint64(lbn) * sectorSize
	for _, e := range p.metadata {
		if off < uint64(e.length) {
			return p.start + e.location + uint32(off/sectorSize), nil
		}
		off -= uint64(e.length)
	}
	return 0, fmt.Errorf("UDF block %d outside the metadata partition", lbn)
}

// readEntry reads a (extended) file entry at the absolute sector.
func (u *udfFS) readEntry(sector uint32) ([]byte, error) {
	b, err := u.readSectors(sector, 1)
	if err != nil {
		return nil, err
	}
	if id, ok := tagID(b); !ok || (id != tagFileEntry && id != tagExtendedFileEntry) {
		return nil, fmt.Errorf("sector %d: expected UDF file entry", sector)
	}
	return b, nil
}

// entryExtents returns the data extents of a file entry, or the embedded data
// for files stored inside the entry. part is the partition of the entry, used
// for short allocation descriptors.
func (u *udfFS) entryExtents(fe []byte, part uint16) (exts []extent, embedded []byte, err error) {
	var eaLen, adLen, adStart uint32
	if le16(fe) == tagExtendedFileEntry {
		eaLen, adLen, adStart = le32(fe[208:]), le32(fe[212:]), 216
	} else {
		eaLen, adLen, adStart = le32(fe[168:]), le32(fe[172:]), 176
	}
	start := adStart + eaLen
	if uint64(start)+uint64(adLen) > uint64(len(fe)) {
		return nil, nil, errors.New("invalid UDF file entry")
	}
	ads := fe[start : start+adLen]
	infoLen := le64(fe[56:])

	adType := le16(fe[16+18:]) & 7
	switch adType {
	case 3: // data embedded in the entry
		if infoLen > uint64(len(ads)) {
			return nil, nil, errors.New("invalid embedded UDF file data")
		}
		return nil, ads[:infoLen], nil
	case 0, 1:
		exts, err := u.parseADs(ads, adType, part, 0)
		return exts, nil, err
	default:
		return nil, nil, fmt.Errorf("unsupported UDF allocation descriptor type %d", adType)
	}
}

// parseADs decodes short (adType 0) or long (adType 1) allocation descriptors,
// following continuation extents (allocation extent descriptors).
func (u *udfFS) parseADs(ads []byte, adType uint16, part uint16, depth int) ([]extent, error) {
	size := 8
	if adType == 1 {
		size = 16
	}
	var exts []extent
	for len(ads) >= size {
		raw := le32(ads)
		e := extent{length: raw & 0x3fffffff, location: le32(ads[4:]), part: part}
		if adType == 1 {
			e.part = le16(ads[8:])
		}
		ads = ads[size:]
		if e.length == 0 {
			break
		}
		switch raw >> 30 {
		case 0: // recorded and allocated
			exts = append(exts, e)
		case 3: // next extent of allocation descriptors
			if depth > 8 {
				return nil, errors.New("too many UDF allocation extents")
			}
			sec, err := u.sector(e.part, e.location)
			if err != nil {
				return nil, err
			}
			b, err := u.readTag(sec, tagAllocationExtent)
			if err != nil {
				return nil, err
			}
			n := min(int(le32(b[20:])), len(b)-24)
			more, err := u.parseADs(b[24:24+n], adType, part, depth+1)
			if err != nil {
				return nil, err
			}
			return append(exts, more...), nil
		}
	}
	return exts, nil
}

// readICB reads the whole content of the file whose entry is at icb.
func (u *udfFS) readICB(icb longAD) ([]byte, error) {
	sec, err := u.sector(icb.part, icb.location)
	if err != nil {
		return nil, err
	}
	fe, err := u.readEntry(sec)
	if err != nil {
		return nil, err
	}
	exts, embedded, err := u.entryExtents(fe, icb.part)
	if err != nil {
		return nil, err
	}
	if embedded != nil {
		return embedded, nil
	}
	infoLen := le64(fe[56:])
	if infoLen > maxUDFFile {
		return nil, fmt.Errorf("UDF file too large (%d bytes)", infoLen)
	}
	data := make([]byte, 0, infoLen)
	for _, e := range exts {
		if uint64(len(data)) >= infoLen {
			break
		}
		b, err := u.readExtent(e)
		if err != nil {
			return nil, err
		}
		data = append(data, b...)
	}
	if uint64(len(data)) < infoLen {
		return nil, errors.New("truncated UDF file")
	}
	return data[:infoLen], nil
}

// readExtent reads the bytes of one extent. Extents in a metadata partition are
// translated block by block, since they may cross metadata file extents.
func (u *udfFS) readExtent(e extent) ([]byte, error) {
	blocks := (e.length + sectorSize - 1) / sectorSize
	if int(e.part) < len(u.partitions) && u.partitions[e.part].metadata != nil {
		buf := make([]byte, 0, int(blocks)*sectorSize)
		for i := uint32(0); i < blocks; i++ {
			sec, err := u.sector(e.part, e.location+i)
			if err != nil {
				return nil, err
			}
			b, err := u.readSectors(sec, 1)
			if err != nil {
				return nil, err
			}
			buf = append(buf, b...)
		}
		return buf[:e.length], nil
	}
	start, err := u.sector(e.part, e.location)
	if err != nil {
		return nil, err
	}
	b, err := u.readSectors(start, blocks)
	if err != nil {
		return nil, err
	}
	return b[:e.length], nil
}

type udfDirEntry struct {
	name  string
	isDir bool
	icb   longAD
}

func (u *udfFS) readDirICB(icb longAD) ([]udfDirEntry, error) {
	data, err := u.readICB(icb)
	if err != nil {
		return nil, err
	}
	var entries []udfDirEntry
	for len(data) >= 38 {
		if id, ok := tagID(data); !ok || id != tagFileIdentifier {
			return nil, errors.New("invalid UDF directory")
		}
		chars := data[18]
		nameLen := int(data[19])
		iuLen := int(le16(data[36:]))
		total := (38 + iuLen + nameLen + 3) &^ 3
		if 38+iuLen+nameLen > len(data) {
			return nil, errors.New("invalid UDF file identifier")
		}
		if chars&(4|8) == 0 { // not deleted, not the parent entry
			entries = append(entries, udfDirEntry{
				name:  cs0(data[38+iuLen : 38+iuLen+nameLen]),
				isDir: chars&2 != 0,
				icb:   parseLongAD(data[20:]),
			})
		}
		if total > len(data) {
			break
		}
		data = data[total:]
	}
	return entries, nil
}

// lookup resolves a slash-separated path (case-insensitive) to its entry.
func (u *udfFS) lookup(name string) (udfDirEntry, error) {
	cur := udfDirEntry{name: "/", isDir: true, icb: u.root}
	for _, elem := range strings.Split(path.Clean("/"+name), "/") {
		if elem == "" {
			continue
		}
		if !cur.isDir {
			return udfDirEntry{}, fmt.Errorf("%s: not a directory", name)
		}
		entries, err := u.readDirICB(cur.icb)
		if err != nil {
			return udfDirEntry{}, err
		}
		found := false
		for _, e := range entries {
			if strings.EqualFold(e.name, elem) {
				cur, found = e, true
				break
			}
		}
		if !found {
			return udfDirEntry{}, fmt.Errorf("%s: %w", name, os.ErrNotExist)
		}
	}
	return cur, nil
}

func (u *udfFS) ReadFile(name string) ([]byte, error) {
	e, err := u.lookup(name)
	if err != nil {
		return nil, err
	}
	if e.isDir {
		return nil, fmt.Errorf("%s: is a directory", name)
	}
	return u.readICB(e.icb)
}

func (u *udfFS) ReadDir(name string) ([]string, error) {
	e, err := u.lookup(name)
	if err != nil {
		return nil, err
	}
	if !e.isDir {
		return nil, fmt.Errorf("%s: not a directory", name)
	}
	entries, err := u.readDirICB(e.icb)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.name
	}
	return names, nil
}

// ---- helpers ----

func parseLongAD(b []byte) longAD {
	return longAD{length: le32(b) & 0x3fffffff, location: le32(b[4:]), part: le16(b[8:])}
}

// cs0 decodes an OSTA compressed unicode string (first byte: 8 or 16 bits per char).
func cs0(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	switch b[0] {
	case 8:
		r := make([]rune, len(b)-1)
		for i, c := range b[1:] {
			r[i] = rune(c)
		}
		return string(r)
	case 16:
		u := make([]uint16, (len(b)-1)/2)
		for i := range u {
			u[i] = binary.BigEndian.Uint16(b[1+2*i:])
		}
		return string(utf16.Decode(u))
	}
	return ""
}

// dstring decodes a fixed-size field whose last byte holds the used length.
func dstring(b []byte) string {
	n := int(b[len(b)-1])
	if n == 0 || n > len(b)-1 {
		return ""
	}
	return strings.TrimSpace(cs0(b[:n]))
}

func le16(b []byte) uint16 { return binary.LittleEndian.Uint16(b) }
func le32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
func le64(b []byte) uint64 { return binary.LittleEndian.Uint64(b) }
