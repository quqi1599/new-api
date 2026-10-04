package dto

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestMediaSurvivesMessageReconstruction(t *testing.T) {
	media := []MediaContent{{Type: ContentTypeText, Text: "describe"}, {Type: ContentTypeImageURL, ImageUrl: &MessageImageUrl{Url: "https://fixture.invalid/image.png"}}, {Type: ContentTypeFile, File: &MessageFile{FileName: "a.pdf", FileData: "fixture"}}}
	var source Message
	source.SetMediaContent(media)
	copy := Message{Role: "user", Content: source.Content}
	require.Equal(t, media, copy.ParseContent())
	copy.SetStringContent("new text")
	require.Equal(t, []MediaContent{{Type: ContentTypeText, Text: "new text"}}, copy.ParseContent())
}

func TestGetFileRecognizesCanonicalFilename(t *testing.T) {
	for _, key := range []string{"filename", "file_name"} {
		part := MediaContent{Type: ContentTypeFile, File: map[string]any{key: "notes.txt", "file_data": "YWJj"}}
		require.Equal(t, "notes.txt", part.GetFile().FileName)
	}
}
