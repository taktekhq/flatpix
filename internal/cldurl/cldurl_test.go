package cldurl

import (
	"errors"
	"reflect"
	"testing"
)

func p(key, value string) Param { return Param{Key: key, Value: value} }

// The URLs below come from Cloudinary's public transformation documentation
// (image transformations, resizing and cropping, chained transformations,
// fetch remote images). The expected parse is written by hand.
func TestParse(t *testing.T) {
	tests := []struct {
		name  string
		path  string
		query string
		want  Request
	}{
		{
			name: "plain delivery, no transformation",
			path: "/demo/image/upload/sample.jpg",
			want: Request{Cloud: "demo", Kind: Upload, PublicID: "sample", Ext: "jpg"},
		},
		{
			name: "resize and crop in one component",
			path: "/demo/image/upload/w_400,h_300,c_fill/sample.jpg",
			want: Request{Cloud: "demo", Kind: Upload, PublicID: "sample", Ext: "jpg",
				Chain: [][]Param{{p("w", "400"), p("h", "300"), p("c", "fill")}}},
		},
		{
			name: "decimal width is kept as text",
			path: "/demo/image/upload/c_scale,w_0.5/sample.jpg",
			want: Request{Cloud: "demo", Kind: Upload, PublicID: "sample", Ext: "jpg",
				Chain: [][]Param{{p("c", "scale"), p("w", "0.5")}}},
		},
		{
			name: "chain of components with a version",
			path: "/demo/image/upload/c_fill,g_auto,w_400/f_auto/q_auto/v1312461204/sample.jpg",
			want: Request{Cloud: "demo", Kind: Upload, PublicID: "sample", Ext: "jpg", Version: "1312461204",
				Chain: [][]Param{
					{p("c", "fill"), p("g", "auto"), p("w", "400")},
					{p("f", "auto")},
					{p("q", "auto")},
				}},
		},
		{
			name: "version only",
			path: "/demo/image/upload/v1312461204/sample.jpg",
			want: Request{Cloud: "demo", Kind: Upload, PublicID: "sample", Ext: "jpg", Version: "1312461204"},
		},
		{
			name: "docs chain: crop to face, round, scale",
			path: "/demo/image/upload/c_crop,g_face,h_400,w_400/r_max/c_scale,w_200/lady.jpg",
			want: Request{Cloud: "demo", Kind: Upload, PublicID: "lady", Ext: "jpg",
				Chain: [][]Param{
					{p("c", "crop"), p("g", "face"), p("h", "400"), p("w", "400")},
					{p("r", "max")},
					{p("c", "scale"), p("w", "200")},
				}},
		},
		{
			name: "folders in the public id",
			path: "/demo/image/upload/w_300/products/shoes/red.png",
			want: Request{Cloud: "demo", Kind: Upload, PublicID: "products/shoes/red", Ext: "png",
				Chain: [][]Param{{p("w", "300")}}},
		},
		{
			name: "aspect ratio value keeps the colon",
			path: "/demo/image/upload/ar_16:9,c_fill,w_800/docs/sample.jpg",
			want: Request{Cloud: "demo", Kind: Upload, PublicID: "docs/sample", Ext: "jpg",
				Chain: [][]Param{{p("ar", "16:9"), p("c", "fill"), p("w", "800")}}},
		},
		{
			name: "background rgb value",
			path: "/demo/image/upload/b_rgb:ff0000,c_pad,h_300,w_300/sample.png",
			want: Request{Cloud: "demo", Kind: Upload, PublicID: "sample", Ext: "png",
				Chain: [][]Param{{p("b", "rgb:ff0000"), p("c", "pad"), p("h", "300"), p("w", "300")}}},
		},
		{
			name: "effect",
			path: "/demo/image/upload/e_grayscale/sample.jpg",
			want: Request{Cloud: "demo", Kind: Upload, PublicID: "sample", Ext: "jpg",
				Chain: [][]Param{{p("e", "grayscale")}}},
		},
		{
			name: "angle",
			path: "/demo/image/upload/a_90/sample.jpg",
			want: Request{Cloud: "demo", Kind: Upload, PublicID: "sample", Ext: "jpg",
				Chain: [][]Param{{p("a", "90")}}},
		},
		{
			name: "quality with a colon value, no extension",
			path: "/demo/image/upload/q_auto:eco,f_auto/sample",
			want: Request{Cloud: "demo", Kind: Upload, PublicID: "sample",
				Chain: [][]Param{{p("q", "auto:eco"), p("f", "auto")}}},
		},
		{
			name: "dpr",
			path: "/demo/image/upload/dpr_2.0,w_200/sample.jpg",
			want: Request{Cloud: "demo", Kind: Upload, PublicID: "sample", Ext: "jpg",
				Chain: [][]Param{{p("dpr", "2.0"), p("w", "200")}}},
		},
		{
			name: "a dot inside a folder name is not an extension",
			path: "/demo/image/upload/v2/release.1/hero.webp",
			want: Request{Cloud: "demo", Kind: Upload, PublicID: "release.1/hero", Ext: "webp", Version: "2"},
		},
		{
			name: "unknown extension stays in the public id",
			path: "/demo/image/upload/archive.tar.gz",
			want: Request{Cloud: "demo", Kind: Upload, PublicID: "archive.tar.gz"},
		},
		{
			name: "extension is lower-cased",
			path: "/demo/image/upload/Photo.JPG",
			want: Request{Cloud: "demo", Kind: Upload, PublicID: "Photo", Ext: "jpg"},
		},
		{
			name: "folder that is not a transformation, no version",
			path: "/demo/image/upload/w_300/Holiday Photos/beach.jpg",
			want: Request{Cloud: "demo", Kind: Upload, PublicID: "Holiday Photos/beach", Ext: "jpg",
				Chain: [][]Param{{p("w", "300")}}},
		},
		{
			name: "version resolves a folder that looks like a parameter",
			path: "/demo/image/upload/w_300/v1/us_east/pic.jpg",
			want: Request{Cloud: "demo", Kind: Upload, PublicID: "us_east/pic", Ext: "jpg", Version: "1",
				Chain: [][]Param{{p("w", "300")}}},
		},
		{
			name: "signature segment is parsed and kept aside",
			path: "/demo/image/upload/s--AbCdEf12--/w_300/sample.jpg",
			want: Request{Cloud: "demo", Kind: Upload, PublicID: "sample", Ext: "jpg", Signature: "AbCdEf12",
				Chain: [][]Param{{p("w", "300")}}},
		},
		{
			name: "analytics query from the SDKs is dropped",
			path: "/demo/image/upload/sample.jpg", query: "_a=DATAAA",
			want: Request{Cloud: "demo", Kind: Upload, PublicID: "sample", Ext: "jpg"},
		},
		{
			name: "fetch, no transformation",
			path: "/demo/image/fetch/https://upload.wikimedia.org/wikipedia/commons/0/01/Flower_poster.jpg",
			want: Request{Cloud: "demo", Kind: Fetch,
				FetchURL: "https://upload.wikimedia.org/wikipedia/commons/0/01/Flower_poster.jpg"},
		},
		{
			name: "fetch with a transformation",
			path: "/demo/image/fetch/w_200,h_200,c_fill/https://example.com/a/b.png",
			want: Request{Cloud: "demo", Kind: Fetch, FetchURL: "https://example.com/a/b.png",
				Chain: [][]Param{{p("w", "200"), p("h", "200"), p("c", "fill")}}},
		},
		{
			name: "fetch with a chain",
			path: "/demo/image/fetch/c_fill,w_300/f_auto/q_auto/http://example.com/x.jpg",
			want: Request{Cloud: "demo", Kind: Fetch, FetchURL: "http://example.com/x.jpg",
				Chain: [][]Param{{p("c", "fill"), p("w", "300")}, {p("f", "auto")}, {p("q", "auto")}}},
		},
		{
			name: "fetch URL with its own query string",
			path: "/demo/image/fetch/https://example.com/img.php", query: "id=7&size=big",
			want: Request{Cloud: "demo", Kind: Fetch, FetchURL: "https://example.com/img.php?id=7&size=big"},
		},
		{
			name: "fetch URL whose double slash was merged by a proxy",
			path: "/demo/image/fetch/w_100/https:/example.com/a.jpg",
			want: Request{Cloud: "demo", Kind: Fetch, FetchURL: "https://example.com/a.jpg",
				Chain: [][]Param{{p("w", "100")}}},
		},
		{
			name: "fetch URL that is itself a Cloudinary-looking path",
			path: "/demo/image/fetch/https://example.com/w_300/x.jpg",
			want: Request{Cloud: "demo", Kind: Fetch, FetchURL: "https://example.com/w_300/x.jpg"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.path, tc.query)
			if err != nil {
				t.Fatalf("Parse(%q): unexpected error: %v", tc.path, err)
			}
			if !reflect.DeepEqual(*got, tc.want) {
				t.Errorf("Parse(%q)\n got  %+v\n want %+v", tc.path, *got, tc.want)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		status int
		msg    string
	}{
		{"empty", "/", 404, ""},
		{"only a cloud", "/demo", 404, ""},
		{"not an image path", "/demo/video/upload/clip.mp4", 400, "image/upload"},
		{"unsupported delivery type", "/demo/image/private/sample.jpg", 400, "private"},
		{"authenticated type", "/demo/image/authenticated/sample.jpg", 400, "authenticated"},
		{"upload without a public id", "/demo/image/upload/", 400, "public id"},
		{"only transformations", "/demo/image/upload/w_300/", 400, "public id"},
		{"version without a public id", "/demo/image/upload/v123/", 400, "public id"},
		{"bad component before version", "/demo/image/upload/folder/v123/x.jpg", 400, "folder"},
		{"empty parameter", "/demo/image/upload/w_300,,h_200/x.jpg", 400, "empty"},
		{"parameter without a value", "/demo/image/upload/w_/x.jpg", 400, "w_"},
		{"fetch without a url", "/demo/image/fetch/", 400, "url"},
		{"fetch with a non http url", "/demo/image/fetch/ftp://example.com/a.jpg", 400, "http"},
		{"fetch with file scheme", "/demo/image/fetch/file:///etc/passwd", 400, "http"},
		{"fetch with only transformations", "/demo/image/fetch/w_300/", 400, "url"},
		{"public id with dot dot", "/demo/image/upload/../../etc/passwd", 400, "invalid"},
		{"public id with a dot dot segment", "/demo/image/upload/a/../b.jpg", 400, "invalid"},
		{"nul byte", "/demo/image/upload/a\x00b.jpg", 400, "invalid"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.path, "")
			if err == nil {
				t.Fatalf("Parse(%q) = %+v, want an error", tc.path, got)
			}
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("error %T is not a *ParseError", err)
			}
			if pe.Status != tc.status {
				t.Errorf("status = %d, want %d (%v)", pe.Status, tc.status, err)
			}
			if tc.msg != "" && !contains(pe.Msg, tc.msg) {
				t.Errorf("message %q does not mention %q", pe.Msg, tc.msg)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
