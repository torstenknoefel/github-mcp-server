package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	ghErrors "github.com/github/github-mcp-server/v2/pkg/errors"
	"github.com/github/github-mcp-server/v2/pkg/inventory"
	"github.com/github/github-mcp-server/v2/pkg/translations"
	"github.com/github/github-mcp-server/v2/pkg/utils"
	"github.com/google/go-github/v92/github"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// PushBinaryFileInput is one file for push_binary_files. The content is base64 so
// that any bytes (images, archives, fonts...) survive the text-only tool interface.
type PushBinaryFileInput struct {
	Path          string  `json:"path"`
	ContentBase64 *string `json:"content_base64"`
}

// PushBinaryFilesInput is the input of push_binary_files.
type PushBinaryFilesInput struct {
	Owner   string                `json:"owner"`
	Repo    string                `json:"repo"`
	Branch  string                `json:"branch"`
	Message string                `json:"message"`
	Files   []PushBinaryFileInput `json:"files"`
}

// blobCreateRequest and blobCreateResponse describe POST /repos/{owner}/{repo}/git/blobs.
type blobCreateRequest struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

type blobCreateResponse struct {
	SHA string `json:"sha"`
}

// PushBinaryFiles creates a tool that pushes files given as base64 in a single commit.
// It works like push_files, but each file's bytes are first stored as a git blob
// (base64-encoded on the wire), so binary files are committed byte for byte.
func PushBinaryFiles(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool[PushBinaryFilesInput, *RepositoryReferenceOutput](
		ToolsetMetadataRepos,
		mcp.Tool{
			Name:        "push_binary_files",
			Description: t("TOOL_PUSH_BINARY_FILES_DESCRIPTION", "Push multiple files, including binary files such as images, to a GitHub repository in a single commit. File content is given as base64."),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_PUSH_BINARY_FILES_USER_TITLE", "Push binary files to repository"),
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
					"branch": {
						Type:        "string",
						Description: "Branch to push to",
					},
					"files": {
						Type:        "array",
						Description: "Array of file objects to push, each object with path (string) and content_base64 (string, the file's bytes encoded as standard base64)",
						Items: &jsonschema.Schema{
							Type:                 "object",
							AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
							Properties: map[string]*jsonschema.Schema{
								"path": {
									Type:        "string",
									Description: "path to the file",
								},
								"content_base64": {
									Type:        "string",
									Description: "file content as standard base64",
								},
							},
							Required: []string{"path", "content_base64"},
						},
					},
					"message": {
						Type:        "string",
						Description: "Commit message",
					},
				},
				Required: []string{"owner", "repo", "branch", "files", "message"},
			},
		},
		publicRepositoryWriteScopeAccess(),
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, input PushBinaryFilesInput) (*mcp.CallToolResult, *RepositoryReferenceOutput, error) {
			owner := input.Owner
			if owner == "" {
				return utils.NewToolResultError("missing required parameter: owner"), nil, nil
			}
			repo := input.Repo
			if repo == "" {
				return utils.NewToolResultError("missing required parameter: repo"), nil, nil
			}
			branch := input.Branch
			if branch == "" {
				return utils.NewToolResultError("missing required parameter: branch"), nil, nil
			}
			message := input.Message
			if message == "" {
				return utils.NewToolResultError("missing required parameter: message"), nil, nil
			}
			if len(input.Files) == 0 {
				return utils.NewToolResultError("files parameter must be a non-empty array of objects with path and content_base64"), nil, nil
			}

			paths := make([]string, 0, len(input.Files))
			contents := make([]string, 0, len(input.Files))
			for _, file := range input.Files {
				if file.Path == "" {
					return utils.NewToolResultError("each file must have a path"), nil, nil
				}
				filePath, err := validateRelativePath(file.Path)
				if err != nil {
					return utils.NewToolResultError(fmt.Sprintf("invalid file path: %s", err)), nil, nil
				}
				if file.ContentBase64 == nil {
					return utils.NewToolResultError("each file must have content_base64"), nil, nil
				}
				encoded := strings.TrimSpace(*file.ContentBase64)
				if _, err := base64.StdEncoding.DecodeString(encoded); err != nil {
					return utils.NewToolResultError(fmt.Sprintf("content_base64 of %s is not valid base64: %s", filePath, err)), nil, nil
				}
				paths = append(paths, filePath)
				contents = append(contents, encoded)
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get GitHub client: %w", err)
			}

			// Store every file as a blob, then reference the blobs from the tree.
			entries := make([]*github.TreeEntry, 0, len(paths))
			for i, filePath := range paths {
				sha, err := createBase64Blob(ctx, client, owner, repo, contents[i])
				if err != nil {
					return utils.NewToolResultError(fmt.Sprintf("failed to create blob for %s: %v", filePath, err)), nil, nil
				}
				entries = append(entries, &github.TreeEntry{
					Path: new(filePath),
					Mode: new("100644"),
					Type: new("blob"),
					SHA:  new(sha),
				})
			}

			// Get the reference for the branch
			var repositoryIsEmpty bool
			var branchNotFound bool
			ref, resp, err := client.Git.GetRef(ctx, owner, repo, "refs/heads/"+branch)
			if err != nil {
				ghErr, isGhErr := err.(*github.ErrorResponse)
				if isGhErr {
					if ghErr.Response.StatusCode == http.StatusConflict && ghErr.Message == "Git Repository is empty." {
						repositoryIsEmpty = true
					} else if ghErr.Response.StatusCode == http.StatusNotFound {
						branchNotFound = true
					}
				}

				if !repositoryIsEmpty && !branchNotFound {
					return ghErrors.NewGitHubAPIErrorResponse(ctx,
						"failed to get branch reference",
						resp,
						err,
					), nil, nil
				}
			}
			if resp != nil && resp.Body != nil {
				defer func() { _ = resp.Body.Close() }()
			}

			var baseCommit *github.Commit
			if !repositoryIsEmpty {
				if branchNotFound {
					ref, err = createReferenceFromDefaultBranch(ctx, client, owner, repo, branch)
					if err != nil {
						return utils.NewToolResultError(fmt.Sprintf("failed to create branch from default: %v", err)), nil, nil
					}
				}

				baseCommit, resp, err = client.Git.GetCommit(ctx, owner, repo, *ref.Object.SHA)
				if err != nil {
					return ghErrors.NewGitHubAPIErrorResponse(ctx,
						"failed to get base commit",
						resp,
						err,
					), nil, nil
				}
				if resp != nil && resp.Body != nil {
					defer func() { _ = resp.Body.Close() }()
				}
			} else {
				var base *github.Commit
				ref, base, err = initializeRepository(ctx, client, owner, repo)
				if err != nil {
					return utils.NewToolResultError(fmt.Sprintf("failed to initialize repository: %v", err)), nil, nil
				}

				defaultBranch := strings.TrimPrefix(*ref.Ref, "refs/heads/")
				if branch != defaultBranch {
					ref, err = createReferenceFromDefaultBranch(ctx, client, owner, repo, branch)
					if err != nil {
						return utils.NewToolResultError(fmt.Sprintf("failed to create branch from default: %v", err)), nil, nil
					}
				}

				baseCommit = base
			}

			newTree, resp, err := client.Git.CreateTree(ctx, owner, repo, *baseCommit.Tree.SHA, entries)
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx,
					"failed to create tree",
					resp,
					err,
				), nil, nil
			}
			if resp != nil && resp.Body != nil {
				defer func() { _ = resp.Body.Close() }()
			}

			commit := github.Commit{
				Message: new(message),
				Tree:    newTree,
				Parents: []*github.Commit{{SHA: baseCommit.SHA}},
			}
			newCommit, resp, err := client.Git.CreateCommit(ctx, owner, repo, commit, nil)
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx,
					"failed to create commit",
					resp,
					err,
				), nil, nil
			}
			if resp != nil && resp.Body != nil {
				defer func() { _ = resp.Body.Close() }()
			}

			ref.Object.SHA = newCommit.SHA
			updatedRef, resp, err := client.Git.UpdateRef(ctx, owner, repo, *ref.Ref, github.UpdateRef{
				SHA:   *newCommit.SHA,
				Force: new(false),
			})
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx,
					"failed to update reference",
					resp,
					err,
				), nil, nil
			}
			defer func() { _ = resp.Body.Close() }()

			r, err := json.Marshal(updatedRef)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to marshal response: %w", err)
			}

			return utils.NewToolResultText(string(r)), repositoryReferenceOutput(updatedRef), nil
		},
	)
}

// createBase64Blob stores base64 content as a git blob and returns its SHA.
func createBase64Blob(ctx context.Context, client *github.Client, owner, repo, contentBase64 string) (string, error) {
	req, err := client.NewRequest(
		ctx,
		http.MethodPost,
		fmt.Sprintf("repos/%s/%s/git/blobs", owner, repo),
		&blobCreateRequest{Content: contentBase64, Encoding: "base64"},
	)
	if err != nil {
		return "", err
	}
	var out blobCreateResponse
	resp, err := client.Do(req, &out)
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil {
		return "", err
	}
	if out.SHA == "" {
		return "", fmt.Errorf("GitHub returned no blob SHA")
	}
	return out.SHA, nil
}
