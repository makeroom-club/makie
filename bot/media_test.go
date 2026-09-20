package bot

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/Southclaws/storyden/app/transports/http/openapi"
	"github.com/Southclaws/storyden/lib/plugin/rpc"
	"github.com/bwmarrin/discordgo"
	"github.com/rs/xid"
)

type recordingAssetUploader struct {
	t          *testing.T
	wantBodies [][]byte
	assetIDs   []xid.ID
	calls      int
}

func (u *recordingAssetUploader) AssetUploadWithBodyWithResponse(
	ctx context.Context,
	params *openapi.AssetUploadParams,
	contentType string,
	body io.Reader,
	reqEditors ...openapi.RequestEditorFn,
) (*openapi.AssetUploadResponse, error) {
	u.t.Helper()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://storyden.test/api/assets", nil)
	if err != nil {
		u.t.Fatal(err)
	}
	for _, edit := range reqEditors {
		if err := edit(ctx, request); err != nil {
			u.t.Fatalf("request editor: %v", err)
		}
	}

	gotBody, err := io.ReadAll(body)
	if err != nil {
		u.t.Fatalf("read upload body: %v", err)
	}
	wantBody := u.wantBodies[u.calls]
	if !reflect.DeepEqual(gotBody, wantBody) {
		u.t.Fatalf("upload body = %v, want %v", gotBody, wantBody)
	}
	if params.ContentLength != int64(len(wantBody)) {
		u.t.Fatalf("ContentLength param = %d, want %d", params.ContentLength, len(wantBody))
	}
	if request.ContentLength != int64(len(wantBody)) {
		u.t.Fatalf("HTTP Content-Length = %d, want %d", request.ContentLength, len(wantBody))
	}
	if contentType != "image/png" {
		u.t.Fatalf("content type = %q, want image/png", contentType)
	}

	assetID := u.assetIDs[u.calls]
	u.calls++
	return &openapi.AssetUploadResponse{
		HTTPResponse: &http.Response{StatusCode: http.StatusOK, Status: "200 OK"},
		JSON200:      &openapi.Asset{Id: assetID.String()},
	}, nil
}

func TestUploadDiscordImageMediaUploadsAllImagesInOrder(t *testing.T) {
	pngOne := []byte("\x89PNG\r\n\x1a\none")
	pngTwo := []byte("\x89PNG\r\n\x1a\ntwo")
	downloads := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/one.png":
			_, _ = response.Write(pngOne)
		case "/two.png":
			_, _ = response.Write(pngTwo)
		default:
			http.NotFound(response, request)
		}
	}))
	defer downloads.Close()

	firstID := xid.New()
	secondID := xid.New()
	uploader := &recordingAssetUploader{
		t:          t,
		wantBodies: [][]byte{pngOne, pngTwo},
		assetIDs:   []xid.ID{firstID, secondID},
	}
	attachments := []*discordgo.MessageAttachment{
		// Discord's size metadata can differ slightly from the CDN response. The
		// upload must use the bytes actually downloaded.
		{Filename: "one.png", ContentType: "image/png", URL: downloads.URL + "/one.png", Size: len(pngOne) - 1},
		{Filename: "notes.txt", ContentType: "text/plain", URL: downloads.URL + "/notes.txt", Size: 4},
		{Filename: "two.png", ContentType: "image/png", URL: downloads.URL + "/two.png", Size: len(pngTwo)},
	}

	got, err := uploadDiscordImageMedia(context.Background(), downloads.Client(), uploader, attachments)
	if err != nil {
		t.Fatalf("uploadDiscordImageMedia() error = %v", err)
	}
	want := []rpc.RobotRunMedia{
		{Type: rpc.RobotRunMediaTypeImage, AssetID: firstID},
		{Type: rpc.RobotRunMediaTypeImage, AssetID: secondID},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("media = %#v, want %#v", got, want)
	}
	if uploader.calls != 2 {
		t.Fatalf("upload calls = %d, want 2", uploader.calls)
	}
}

func TestDiscordImageAttachmentsForTriggerUsesOnlyTriggerAndReferencedMessage(t *testing.T) {
	triggerImage := &discordgo.MessageAttachment{ID: "trigger-image"}
	referencedImage := &discordgo.MessageAttachment{ID: "referenced-image"}
	trigger := &discordgo.Message{
		Author:      &discordgo.User{ID: "asking-user"},
		Attachments: []*discordgo.MessageAttachment{triggerImage},
	}
	referenced := &discordgo.Message{
		Author:      &discordgo.User{ID: "other-user"},
		Attachments: []*discordgo.MessageAttachment{referencedImage},
	}

	got := discordImageAttachmentsForTrigger(true, trigger, referenced, testBotID)
	if want := []*discordgo.MessageAttachment{triggerImage, referencedImage}; !reflect.DeepEqual(got, want) {
		t.Fatalf("attachments = %#v, want %#v", got, want)
	}

	if got := discordImageAttachmentsForTrigger(false, trigger, referenced, testBotID); got != nil {
		t.Fatalf("unmentioned trigger attachments = %#v, want nil", got)
	}

	referenced.Author.ID = testBotID
	if got := discordImageAttachmentsForTrigger(true, trigger, referenced, testBotID); !reflect.DeepEqual(got, []*discordgo.MessageAttachment{triggerImage}) {
		t.Fatalf("bot-reference attachments = %#v, want only trigger image", got)
	}
}

func TestBuildRobotMessagesForRunComposesMessagesWithoutHistoricMedia(t *testing.T) {
	trigger := &discordgo.Message{
		ID:      "trigger",
		Author:  &discordgo.User{ID: "asking-user"},
		Content: "<@" + testBotID + "> what is this?",
	}
	historic := &discordgo.Message{
		ID:      "historic",
		Author:  &discordgo.User{ID: "other-user"},
		Content: "an older message",
		Attachments: []*discordgo.MessageAttachment{
			{Filename: "historic.png", ContentType: "image/png"},
		},
	}
	app := &PluginApp{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	got := app.buildRobotMessagesForRun(
		context.Background(),
		conversationRun{Messages: []*discordgo.Message{trigger, historic}},
		testBotID,
		&discordgo.MessageCreate{Message: trigger},
		nil,
	)
	if len(got) != 2 {
		t.Fatalf("message count = %d, want 2", len(got))
	}
	for _, message := range got {
		if len(message.Media) != 0 {
			t.Fatalf("message %q media = %#v, want none", message.Content, message.Media)
		}
	}
}

func TestUploadDiscordImageRejectsNonImageResponse(t *testing.T) {
	payload := []byte("this is not an image")
	download := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write(payload)
	}))
	defer download.Close()

	uploader := &recordingAssetUploader{t: t}
	_, err := uploadDiscordImage(
		context.Background(),
		download.Client(),
		uploader,
		&discordgo.MessageAttachment{
			Filename:    "spoofed.png",
			ContentType: "image/png",
			URL:         download.URL,
			Size:        len(payload),
		},
	)
	if err == nil {
		t.Fatal("uploadDiscordImage() error = nil, want unsupported media error")
	}
	if uploader.calls != 0 {
		t.Fatalf("upload calls = %d, want 0", uploader.calls)
	}
}
