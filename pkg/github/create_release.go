package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/github/github-mcp-server/v2/pkg/inventory"
	"github.com/github/github-mcp-server/v2/pkg/translations"
	"github.com/github/github-mcp-server/v2/pkg/utils"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// CreateReleaseInput is the input of create_release.
type CreateReleaseInput struct {
	Owner           string `json:"owner"`
	Repo            string `json:"repo"`
	TagName         string `json:"tag_name"`
	TargetCommitish string `json:"target_commitish,omitempty"`
	Name            string `json:"name,omitempty"`
	Body            string `json:"body,omitempty"`
	Draft           bool   `json:"draft,omitempty"`
	Prerelease      bool   `json:"prerelease,omitempty"`
}

// CreateReleaseOutput is what create_release returns.
type CreateReleaseOutput struct {
	ID              int64  `json:"id"`
	TagName         string `json:"tag_name"`
	TargetCommitish string `json:"target_commitish,omitempty"`
	Name            string `json:"name,omitempty"`
	HTMLURL         string `json:"html_url,omitempty"`
	Draft           bool   `json:"draft"`
	Prerelease      bool   `json:"prerelease"`
}

// releaseCreateRequest is the body of POST /repos/{owner}/{repo}/releases.
type releaseCreateRequest struct {
	TagName         string `json:"tag_name"`
	TargetCommitish string `json:"target_commitish,omitempty"`
	Name            string `json:"name,omitempty"`
	Body            string `json:"body,omitempty"`
	Draft           bool   `json:"draft"`
	Prerelease      bool   `json:"prerelease"`
}

// CreateRelease creates a tool that publishes (or drafts) a GitHub release.
// GitHub creates the tag from target_commitish when the release is published.
func CreateRelease(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool[CreateReleaseInput, *CreateReleaseOutput](
		ToolsetMetadataRepos,
		mcp.Tool{
			Name:        "create_release",
			Description: t("TOOL_CREATE_RELEASE_DESCRIPTION", "Create a release in a GitHub repository. The tag is created from target_commitish (default: the default branch) when the release is published."),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_CREATE_RELEASE_USER_TITLE", "Create release"),
				ReadOnlyHint: false,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"owner": {
						Type:        "string",
						Description: "Repository owner",
					},
					"repo": {
						Type:        "string",
						Description: "Repository name",
					},
					"tag_name": {
						Type:        "string",
						Description: "Name of the tag, for example v1.2.0",
					},
					"target_commitish": {
						Type:        "string",
						Description: "Branch or commit SHA the tag is created from if it does not exist yet. Defaults to the repository's default branch",
					},
					"name": {
						Type:        "string",
						Description: "Title of the release",
					},
					"body": {
						Type:        "string",
						Description: "Release notes (Markdown)",
					},
					"draft": {
						Type:        "boolean",
						Description: "Create an unpublished draft instead of publishing (default false)",
					},
					"prerelease": {
						Type:        "boolean",
						Description: "Mark the release as a pre-release (default false)",
					},
				},
				Required: []string{"owner", "repo", "tag_name"},
			},
		},
		publicRepositoryWriteScopeAccess(),
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, input CreateReleaseInput) (*mcp.CallToolResult, *CreateReleaseOutput, error) {
			if input.Owner == "" {
				return utils.NewToolResultError("missing required parameter: owner"), nil, nil
			}
			if input.Repo == "" {
				return utils.NewToolResultError("missing required parameter: repo"), nil, nil
			}
			if input.TagName == "" {
				return utils.NewToolResultError("missing required parameter: tag_name"), nil, nil
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get GitHub client: %w", err)
			}

			req, err := client.NewRequest(
				ctx,
				http.MethodPost,
				fmt.Sprintf("repos/%s/%s/releases", input.Owner, input.Repo),
				&releaseCreateRequest{
					TagName:         input.TagName,
					TargetCommitish: input.TargetCommitish,
					Name:            input.Name,
					Body:            input.Body,
					Draft:           input.Draft,
					Prerelease:      input.Prerelease,
				},
			)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("failed to build request: %v", err)), nil, nil
			}

			out := &CreateReleaseOutput{}
			resp, err := client.Do(req, out)
			if resp != nil && resp.Body != nil {
				defer func() { _ = resp.Body.Close() }()
			}
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("failed to create release: %v", err)), nil, nil
			}

			r, err := json.Marshal(out)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to marshal response: %w", err)
			}
			return utils.NewToolResultText(string(r)), out, nil
		},
	)
}
