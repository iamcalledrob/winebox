package icons

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
)

// Written by Claudio, simplified a bit by hand

const (
	rtIcon      = 3
	rtGroupIcon = 14
)

// ExtractBestIcon returns the best available icon from a Windows PE executable.
// Since the icon can be in various formats, the image is normalised to a golang image.Image
func ExtractBestIcon(path string) (image.Image, error) {
	f, err := pe.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening PE: %w", err)
	}
	defer func() { _ = f.Close() }()

	rsrc := f.Section(".rsrc")
	if rsrc == nil {
		return nil, fmt.Errorf("no .rsrc section")
	}

	var data []byte
	data, err = rsrc.Data()
	if err != nil {
		return nil, fmt.Errorf("reading .rsrc: %w", err)
	}
	rva := rsrc.VirtualAddress

	var groupData []byte
	groupData, err = findResource(data, rva, rtGroupIcon, 0)
	if err != nil {
		return nil, fmt.Errorf("finding group icon: %w", err)
	}

	var iconID uint16
	iconID, err = bestIconID(groupData)
	if err != nil {
		return nil, err
	}

	var iconData []byte
	iconData, err = findResource(data, rva, rtIcon, uint32(iconID))
	if err != nil {
		return nil, fmt.Errorf("finding icon resource %d: %w", iconID, err)
	}
	return decodeIconData(iconData)
}

// findResource walks the 3-level PE resource directory and returns raw resource data.
// resID=0 selects the first available entry at level 2 (i.e. the first icon group).
func findResource(rsrc []byte, rva, resType, resID uint32) ([]byte, error) {
	typeOff, err := findSubdir(rsrc, 0, resType)
	if err != nil {
		return nil, fmt.Errorf("resource type %d: %w", resType, err)
	}
	var nameOff uint32
	if resID == 0 {
		nameOff, err = firstSubdir(rsrc, typeOff)
	} else {
		nameOff, err = findSubdir(rsrc, typeOff, resID)
	}
	if err != nil {
		return nil, fmt.Errorf("resource id %d: %w", resID, err)
	}
	var dataOff uint32
	dataOff, err = firstDataEntry(rsrc, nameOff)
	if err != nil {
		return nil, err
	}
	return resourceData(rsrc, rva, dataOff)
}

// findSubdir searches a resource directory for an entry with the given integer ID
// and returns the offset of its subdirectory.
func findSubdir(rsrc []byte, dirOff, id uint32) (uint32, error) {
	if int(dirOff+16) > len(rsrc) {
		return 0, fmt.Errorf("directory out of bounds")
	}
	named := uint32(binary.LittleEndian.Uint16(rsrc[dirOff+12:]))
	ids := uint32(binary.LittleEndian.Uint16(rsrc[dirOff+14:]))
	for i := uint32(0); i < named+ids; i++ {
		e := dirOff + 16 + i*8
		if int(e+8) > len(rsrc) {
			break
		}
		nameOrID := binary.LittleEndian.Uint32(rsrc[e:])
		subdirOff := binary.LittleEndian.Uint32(rsrc[e+4:])
		// Named entries have high bit set; skip them when matching by integer ID.
		if nameOrID&0x80000000 == 0 && nameOrID == id && subdirOff&0x80000000 != 0 {
			return subdirOff &^ 0x80000000, nil
		}
	}
	return 0, fmt.Errorf("id %d not found", id)
}

// firstSubdir returns the subdirectory offset of the first entry in a resource directory.
func firstSubdir(rsrc []byte, dirOff uint32) (uint32, error) {
	if int(dirOff+24) > len(rsrc) { // 16 header + 8 entry
		return 0, fmt.Errorf("directory out of bounds or empty")
	}
	subdirOff := binary.LittleEndian.Uint32(rsrc[dirOff+20:])
	if subdirOff&0x80000000 == 0 {
		return 0, fmt.Errorf("first entry is not a subdirectory")
	}
	return subdirOff &^ 0x80000000, nil
}

// firstDataEntry returns the offset of the first IMAGE_RESOURCE_DATA_ENTRY in a directory.
func firstDataEntry(rsrc []byte, dirOff uint32) (uint32, error) {
	if int(dirOff+24) > len(rsrc) {
		return 0, fmt.Errorf("directory out of bounds or empty")
	}
	dataOff := binary.LittleEndian.Uint32(rsrc[dirOff+20:])
	if dataOff&0x80000000 != 0 {
		return 0, fmt.Errorf("expected data entry, got subdirectory")
	}
	return dataOff, nil
}

// resourceData reads an IMAGE_RESOURCE_DATA_ENTRY and returns the referenced data.
func resourceData(rsrc []byte, rva, entryOff uint32) ([]byte, error) {
	if int(entryOff+8) > len(rsrc) {
		return nil, fmt.Errorf("data entry out of bounds")
	}
	dataRVA := binary.LittleEndian.Uint32(rsrc[entryOff:])
	size := binary.LittleEndian.Uint32(rsrc[entryOff+4:])
	off := dataRVA - rva
	if uint64(off)+uint64(size) > uint64(len(rsrc)) {
		return nil, fmt.Errorf("resource data out of bounds")
	}
	return rsrc[off : off+size], nil
}

// bestIconID parses a GRPICONDIR resource and returns the ID of the best icon entry.
// Prefers larger area; breaks ties by higher bit depth.
func bestIconID(data []byte) (uint16, error) {
	if len(data) < 6 {
		return 0, fmt.Errorf("group icon too small")
	}
	count := int(binary.LittleEndian.Uint16(data[4:]))
	if count == 0 {
		return 0, fmt.Errorf("group icon has no entries")
	}
	if len(data) < 6+count*14 {
		return 0, fmt.Errorf("group icon truncated")
	}
	var bestID uint16
	var bestArea, bestBitCount int
	for i := 0; i < count; i++ {
		e := data[6+i*14:]
		w, h := int(e[0]), int(e[1])
		if w == 0 {
			w = 256
		}
		if h == 0 {
			h = 256
		}
		bitCount := int(binary.LittleEndian.Uint16(e[6:]))
		area := w * h
		if area > bestArea || (area == bestArea && bitCount > bestBitCount) {
			bestArea = area
			bestBitCount = bitCount
			bestID = binary.LittleEndian.Uint16(e[12:])
		}
	}
	return bestID, nil
}

var pngMagic = []byte("\x89PNG\r\n\x1a\n")

func decodeIconData(data []byte) (image.Image, error) {
	if len(data) >= len(pngMagic) && bytes.Equal(data[:len(pngMagic)], pngMagic) {
		return png.Decode(bytes.NewReader(data))
	}
	return decodeDIB(data)
}

// decodeDIB decodes an RT_ICON DIB (BITMAPINFOHEADER + pixels, no file header).
func decodeDIB(data []byte) (image.Image, error) {
	if len(data) < 40 {
		return nil, fmt.Errorf("DIB too small")
	}
	width := int(int32(binary.LittleEndian.Uint32(data[4:])))
	height := int(int32(binary.LittleEndian.Uint32(data[8:])))
	bitCount := binary.LittleEndian.Uint16(data[14:])

	// Icon DIBs store XOR mask + AND mask stacked vertically; actual height is half.
	if height < 0 {
		height = -height
	}
	height /= 2

	switch bitCount {
	case 32:
		return decode32bpp(data[40:], width, height), nil
	case 24:
		return decode24bpp(data[40:], width, height), nil
	case 8:
		return decodePaletted(data, width, height, 8), nil
	case 4:
		return decodePaletted(data, width, height, 4), nil
	default:
		return nil, fmt.Errorf("unsupported bit depth: %d", bitCount)
	}
}

func decode32bpp(pixels []byte, width, height int) image.Image {
	stride := width * 4
	return decodePixels(pixels, width, height, stride, 4, func(p []byte) color.NRGBA {
		return color.NRGBA{R: p[2], G: p[1], B: p[0], A: p[3]}
	})
}

func decode24bpp(pixels []byte, width, height int) image.Image {
	stride := ((width*3 + 3) / 4) * 4 // rows are DWORD-aligned
	return decodePixels(pixels, width, height, stride, 3, func(p []byte) color.NRGBA {
		return color.NRGBA{R: p[2], G: p[1], B: p[0], A: 255}
	})
}

// decodePaletted handles 4bpp and 8bpp paletted icons with an AND mask for transparency.
func decodePaletted(data []byte, width, height, bpp int) image.Image {
	headerSize := int(binary.LittleEndian.Uint32(data[0:]))
	numColors := int(binary.LittleEndian.Uint32(data[32:]))
	if numColors == 0 {
		numColors = 1 << bpp
	}

	palette := make([]color.NRGBA, numColors)
	for i := 0; i < numColors; i++ {
		off := headerSize + i*4
		if off+4 > len(data) {
			break
		}
		palette[i] = color.NRGBA{B: data[off], G: data[off+1], R: data[off+2], A: 255}
	}

	pixelsPerByte := 8 / bpp
	xorStride := ((width + pixelsPerByte*4 - 1) / (pixelsPerByte * 4)) * 4 // DWORD-aligned
	andStride := ((width + 31) / 32) * 4
	xorStart := headerSize + numColors*4
	andStart := xorStart + xorStride*height

	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		xorRowOff := xorStart + (height-1-y)*xorStride
		andRowOff := andStart + (height-1-y)*andStride
		for x := 0; x < width; x++ {
			var idx int
			if bpp == 4 {
				b := data[xorRowOff+x/2]
				if x%2 == 0 {
					idx = int(b >> 4)
				} else {
					idx = int(b & 0x0f)
				}
			} else { // 8bpp
				idx = int(data[xorRowOff+x])
			}
			c := palette[idx]
			// AND mask: bit 1 = transparent
			if (data[andRowOff+x/8]>>(7-uint(x%8)))&1 == 1 {
				c.A = 0
			}
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

func decodePixels(pixels []byte, width, height, stride, bpp int, pixelColor func(p []byte) color.NRGBA) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		row := pixels[(height-1-y)*stride:]
		for x := 0; x < width; x++ {
			img.SetNRGBA(x, y, pixelColor(row[x*bpp:]))
		}
	}
	return img
}
