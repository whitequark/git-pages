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
	decoder := json.NewDecoder(response.Body)

	var userInfo struct {
		ID    int64
		Login string
	}
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
		Handle: &userInfo.Login,
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
	decoder := json.NewDecoder(response.Body)

	var repositoryInfo struct{ Permissions struct{ Push bool } }
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
