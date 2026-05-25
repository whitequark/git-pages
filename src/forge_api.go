package git_pages

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func makeGogsAPIRequest(
	baseURL *url.URL, authorization string, endpoint string,
) (*http.Request, *http.Response, error) {
	request, err := http.NewRequest("GET", baseURL.ResolveReference(&url.URL{
		Path: fmt.Sprintf("/api/v1/%s", endpoint),
	}).String(), nil)
	if err != nil {
		panic(err) // misconfiguration
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", authorization)

	httpClient := http.Client{Timeout: 5 * time.Second}
	response, err := httpClient.Do(request)
	return request, response, err
}

// Gogs, Gitea, and Forgejo all support the same API here.
func FetchGogsAuthorizedUser(baseURL *url.URL, authorization string) (*ForgeUser, error) {
	request, response, err := makeGogsAPIRequest(baseURL, authorization, "user")
	if err != nil {
		return nil, AuthError{
			http.StatusServiceUnavailable,
			fmt.Sprintf("cannot fetch authorized forge user: %s", err),
		}
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, AuthError{
			http.StatusServiceUnavailable,
			fmt.Sprintf(
				"cannot fetch authorized forge user: GET %s returned %s",
				request.URL,
				response.Status,
			),
		}
	}

	var userInfo struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	decoder := json.NewDecoder(response.Body)
	if err := decoder.Decode(&userInfo); err != nil {
		return nil, errors.Join(AuthError{
			http.StatusServiceUnavailable,
			fmt.Sprintf(
				"cannot fetch authorized forge user: GET %s returned malformed JSON",
				request.URL,
			),
		}, err)
	}

	origin := request.URL.Hostname()
	return &ForgeUser{
		Origin: &origin,
		Id:     &userInfo.ID,
		Name:   &userInfo.Login,
	}, nil
}

// Gogs, Gitea, and Forgejo all support the same API here.
func CheckGogsRepositoryPushPermission(baseURL *url.URL, authorization string) error {
	ownerAndRepo := strings.TrimSuffix(strings.TrimPrefix(baseURL.Path, "/"), ".git")
	request, response, err := makeGogsAPIRequest(baseURL, authorization,
		fmt.Sprintf("repos/%s", ownerAndRepo))
	if err != nil {
		return AuthError{
			http.StatusServiceUnavailable,
			fmt.Sprintf("cannot check repository permissions: %s", err),
		}
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusNotFound {
		return AuthError{
			http.StatusNotFound,
			fmt.Sprintf("no repository %s", ownerAndRepo),
		}
	} else if response.StatusCode == http.StatusUnauthorized {
		return AuthError{
			http.StatusUnauthorized,
			fmt.Sprintf("no access to %s or invalid token", ownerAndRepo),
		}
	} else if response.StatusCode != http.StatusOK {
		return AuthError{
			http.StatusServiceUnavailable,
			fmt.Sprintf(
				"cannot check repository permissions: GET %s returned %s",
				request.URL,
				response.Status,
			),
		}
	}

	var repositoryInfo struct {
		Permissions struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	decoder := json.NewDecoder(response.Body)
	if err := decoder.Decode(&repositoryInfo); err != nil {
		return errors.Join(AuthError{
			http.StatusServiceUnavailable,
			fmt.Sprintf(
				"cannot check repository permissions: GET %s returned malformed JSON",
				request.URL,
			),
		}, err)
	}

	if !repositoryInfo.Permissions.Push {
		return AuthError{
			http.StatusUnauthorized,
			fmt.Sprintf("no push permission for %s", ownerAndRepo),
		}
	}

	// this token authorizes pushing to the repo, yay!
	return nil
}

type ForgeActionRun struct {
	TriggerUser *ForgeUser
	PullRequest *ForgePullRequest // only if `event == "pull_request"`
}

type ForgePullRequest struct {
	OwnerID        int64
	OwnerName      string
	RepositoryID   int64
	RepositoryName string
	Number         int64
}

// This is a Forgejo-specific API added in https://codeberg.org/forgejo/forgejo/pulls/12727
// and available starting in Forgejo vX.Y.
func FetchForgejoActionRun(baseURL *url.URL, authorization string) (*ForgeActionRun, error) {
	request, response, err := makeGogsAPIRequest(baseURL, authorization, "actions/run")
	if err != nil {
		return nil, AuthError{
			http.StatusServiceUnavailable,
			fmt.Sprintf("cannot fetch workflow run's pull request: %s", err),
		}
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusForbidden {
		return nil, AuthError{http.StatusForbidden, "not an automatic actions token"}
	} else if response.StatusCode == http.StatusUnauthorized {
		return nil, AuthError{http.StatusUnauthorized, "malformed token"}
	} else if response.StatusCode == http.StatusNotFound {
		return nil, AuthError{http.StatusInternalServerError, "endpoint not available"}
	} else if response.StatusCode != http.StatusOK {
		return nil, AuthError{
			http.StatusServiceUnavailable,
			fmt.Sprintf(
				"cannot fetch workflow run's pull request: GET %s returned %s",
				request.URL,
				response.Status,
			),
		}
	}

	var runInfo struct {
		Event        string `json:"event"`
		EventPayload string `json:"event_payload"`
		TriggerUser  struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
		} `json:"trigger_user"`
	}
	decoder := json.NewDecoder(response.Body)
	if err := decoder.Decode(&runInfo); err != nil {
		return nil, errors.Join(AuthError{
			http.StatusServiceUnavailable,
			fmt.Sprintf(
				"cannot fetch workflow run's pull request: GET %s returned malformed JSON",
				request.URL,
			),
		}, err)
	}
	origin := request.URL.Hostname()
	actionRun := &ForgeActionRun{
		TriggerUser: &ForgeUser{
			Origin: &origin,
			Id:     &runInfo.TriggerUser.ID,
			Name:   &runInfo.TriggerUser.Username,
		},
	}

	if runInfo.Event == "pull_request" {
		var eventInfo struct {
			Number     int64 `json:"number"`
			Repository struct {
				ID    int64  `json:"id"`
				Name  string `json:"name"`
				Owner struct {
					ID       int64  `json:"id"`
					Username string `json:"username"`
				}
			}
		}
		decoder = json.NewDecoder(strings.NewReader(runInfo.EventPayload))
		if err := decoder.Decode(&eventInfo); err != nil {
			return nil, errors.Join(AuthError{
				http.StatusServiceUnavailable,
				fmt.Sprintf(
					"cannot fetch workflow run's pull request: GET %s returned malformed JSON "+
						"in event payload",
					request.URL,
				),
			}, err)
		}
		actionRun.PullRequest = &ForgePullRequest{
			OwnerID:        eventInfo.Repository.Owner.ID,
			OwnerName:      eventInfo.Repository.Owner.Username,
			RepositoryID:   eventInfo.Repository.ID,
			RepositoryName: eventInfo.Repository.Name,
			Number:         eventInfo.Number,
		}
	} else {
		// we aren't decoding the other events
	}

	return actionRun, nil
}
