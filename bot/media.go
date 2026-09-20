package bot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/Southclaws/storyden/app/transports/http/openapi"
	"github.com/Southclaws/storyden/lib/plugin/rpc"
	"github.com/bwmarrin/discordgo"
	"github.com/rs/xid"
)

const discordImageDownloadTimeout = 20 * time.Second

var discordImageHTTPClient = &http.Client{Timeout: discordImageDownloadTimeout}

type assetUploadClient interface {
	AssetUploadWithBodyWithResponse(
		ctx context.Context,
		params *openapi.AssetUploadParams,
		contentType string,
		body io.Reader,
		reqEditors ...openapi.RequestEditorFn,
	) (*openapi.AssetUploadResponse, error)
}

func (a *PluginApp) buildRobotMessagesForRun(ctx context.Context, run conversationRun, botID string, trigger *discordgo.MessageCreate, imageAttachments []*discordgo.MessageAttachment) []rpc.RobotRunMessage {
	if len(run.Messages) == 0 {
		a.Logger.Debug("ignoring invocation with no new discord messages", slog.String("channel_id", trigger.ChannelID), slog.String("message_id", trigger.ID))
		return nil
	}

	mediaByMessageID, err := a.loadDiscordImageMedia(ctx, trigger.ID, imageAttachments)
	if err != nil {
		a.failConversationSession(run)
		a.Logger.Error("failed to upload discord image attachments", slog.String("error", err.Error()), slog.String("channel_id", trigger.ChannelID), slog.String("message_id", trigger.ID))
		return nil
	}

	return buildRobotMessages(run.Messages, botID, mediaByMessageID)
}

func discordImageAttachmentsForTrigger(mentionsBot bool, trigger, referenced *discordgo.Message, botID string) []*discordgo.MessageAttachment {
	if !mentionsBot || trigger == nil {
		return nil
	}

	attachments := append([]*discordgo.MessageAttachment(nil), trigger.Attachments...)
	if referenced != nil && referenced.Author != nil && referenced.Author.ID != botID {
		attachments = append(attachments, referenced.Attachments...)
	}
	return attachments
}

func (a *PluginApp) loadDiscordImageMedia(ctx context.Context, messageID string, attachments []*discordgo.MessageAttachment) (map[string][]rpc.RobotRunMedia, error) {
	if !hasDiscordImageAttachments(attachments) {
		return nil, nil
	}

	client, err := a.Plugin.BuildAPIClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("build Storyden API client: %w", err)
	}

	media, err := uploadDiscordImageMedia(ctx, discordImageHTTPClient, client, attachments)
	if err != nil {
		return nil, err
	}
	return map[string][]rpc.RobotRunMedia{messageID: media}, nil
}

func hasDiscordImageAttachments(attachments []*discordgo.MessageAttachment) bool {
	for _, attachment := range attachments {
		if discordAttachmentImageType(attachment) != "" {
			return true
		}
	}
	return false
}

func uploadDiscordImageMedia(
	ctx context.Context,
	httpClient *http.Client,
	client assetUploadClient,
	attachments []*discordgo.MessageAttachment,
) ([]rpc.RobotRunMedia, error) {
	mediaItems := make([]rpc.RobotRunMedia, 0, len(attachments))

	for _, attachment := range attachments {
		if discordAttachmentImageType(attachment) == "" {
			continue
		}

		media, err := uploadDiscordImage(ctx, httpClient, client, attachment)
		if err != nil {
			return nil, fmt.Errorf("upload Discord attachment %q: %w", attachment.Filename, err)
		}
		mediaItems = append(mediaItems, media)
	}

	return mediaItems, nil
}

func uploadDiscordImage(
	ctx context.Context,
	httpClient *http.Client,
	client assetUploadClient,
	attachment *discordgo.MessageAttachment,
) (rpc.RobotRunMedia, error) {
	if attachment == nil || attachment.Size <= 0 || strings.TrimSpace(attachment.URL) == "" {
		return rpc.RobotRunMedia{}, errors.New("attachment is missing its URL or size")
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, attachment.URL, nil)
	if err != nil {
		return rpc.RobotRunMedia{}, fmt.Errorf("create download request: %w", err)
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return rpc.RobotRunMedia{}, fmt.Errorf("download attachment: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return rpc.RobotRunMedia{}, fmt.Errorf("download attachment: unexpected status %s", response.Status)
	}

	maxDownloadSize := int64(attachment.Size) + 1<<20
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxDownloadSize+1))
	if err != nil {
		return rpc.RobotRunMedia{}, fmt.Errorf("download attachment body: %w", err)
	}
	if int64(len(payload)) > maxDownloadSize {
		return rpc.RobotRunMedia{}, fmt.Errorf("downloaded attachment exceeded expected size by more than 1 MiB")
	}
	detectedType := http.DetectContentType(payload)
	if !isRobotImageType(detectedType) {
		return rpc.RobotRunMedia{}, fmt.Errorf("downloaded attachment has unsupported media type %q", detectedType)
	}

	filename := strings.TrimSpace(attachment.Filename)
	if filename == "" {
		filename = "discord-image"
	}
	contentLength := int64(len(payload))
	uploadResponse, err := client.AssetUploadWithBodyWithResponse(
		ctx,
		&openapi.AssetUploadParams{
			Filename:      &filename,
			ContentLength: contentLength,
		},
		detectedType,
		bytes.NewReader(payload),
		func(_ context.Context, request *http.Request) error {
			request.ContentLength = contentLength
			return nil
		},
	)
	if err != nil {
		return rpc.RobotRunMedia{}, fmt.Errorf("upload to Storyden: %w", err)
	}
	if uploadResponse.StatusCode() != http.StatusOK || uploadResponse.JSON200 == nil {
		return rpc.RobotRunMedia{}, fmt.Errorf("upload to Storyden: unexpected status %s", uploadResponse.Status())
	}

	assetID, err := xid.FromString(uploadResponse.JSON200.Id)
	if err != nil {
		return rpc.RobotRunMedia{}, fmt.Errorf("parse uploaded asset ID: %w", err)
	}

	return rpc.RobotRunMedia{Type: rpc.RobotRunMediaTypeImage, AssetID: assetID}, nil
}

func discordAttachmentImageType(attachment *discordgo.MessageAttachment) string {
	if attachment == nil {
		return ""
	}

	mediaType, _, _ := mime.ParseMediaType(attachment.ContentType)
	if isRobotImageType(mediaType) {
		return mediaType
	}

	mediaType, _, _ = mime.ParseMediaType(mime.TypeByExtension(strings.ToLower(filepath.Ext(attachment.Filename))))
	if isRobotImageType(mediaType) {
		return mediaType
	}
	return ""
}

func isRobotImageType(mediaType string) bool {
	switch strings.ToLower(mediaType) {
	case "image/gif", "image/jpeg", "image/png", "image/webp":
		return true
	default:
		return false
	}
}
