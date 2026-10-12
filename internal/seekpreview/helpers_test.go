package seekpreview_test

import (
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"testing"
)

func decodeJPEG(t *testing.T, path string) image.Image {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	img, err := jpeg.Decode(fh)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return img
}

func luma(img image.Image, x, y int) int {
	return int(color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y)
}
