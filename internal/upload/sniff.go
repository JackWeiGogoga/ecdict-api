package upload

import "bytes"

// Image is a sniffed image type.
type Image struct {
	ContentType string
	Ext         string
}

var heifBrands = map[string]Image{
	"heic": {"image/heic", ".heic"},
	"heix": {"image/heic", ".heic"},
	"hevc": {"image/heic", ".heic"},
	"heim": {"image/heic", ".heic"},
	"heis": {"image/heic", ".heic"},
	"mif1": {"image/heif", ".heif"},
	"msf1": {"image/heif", ".heif"},
}

// DetectImage identifies JPEG, PNG, WebP and HEIC/HEIF by magic bytes.
func DetectImage(b []byte) (Image, bool) {
	switch {
	case len(b) >= 3 && bytes.Equal(b[:3], []byte{0xFF, 0xD8, 0xFF}):
		return Image{"image/jpeg", ".jpg"}, true
	case len(b) >= 8 && bytes.Equal(b[:8], []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return Image{"image/png", ".png"}, true
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return Image{"image/webp", ".webp"}, true
	case len(b) >= 12 && string(b[4:8]) == "ftyp":
		if img, ok := heifBrands[string(b[8:12])]; ok {
			return img, true
		}
	}
	return Image{}, false
}
