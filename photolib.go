package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

var errPhotoLibSession = errors.New("PhotoLib session is no longer valid")

type photoLibClient struct {
	origin string
	http   *http.Client
}

type photoLibUser struct {
	ID                 int64  `json:"id"`
	Username           string `json:"username"`
	DisplayName        string `json:"displayName"`
	MustChangePassword bool   `json:"mustChangePassword"`
	MFA                struct {
		EnrollmentRequired bool `json:"enrollmentRequired"`
	} `json:"mfa"`
}

type photoLibTokens struct {
	Status       string `json:"status"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
}

type photoLibPairing struct {
	RequestID  string `json:"requestId"`
	DeviceCode string `json:"deviceCode"`
	UserCode   string `json:"userCode"`
	ExpiresIn  int64  `json:"expiresIn"`
	Interval   int64  `json:"interval"`
}

func (p photoLibClient) authorizationURL(requestID string) string {
	return p.origin + "/mcp/authorize?" + url.Values{"request": {requestID}}.Encode()
}

func (p photoLibClient) call(ctx context.Context, path string, input any, bearer string, output any) error {
	method := http.MethodGet
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		method, body = http.MethodPost, bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.origin+"/api/v1/auth"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		switch resp.StatusCode {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict:
			return errPhotoLibSession
		default:
			return fmt.Errorf("PhotoLib returned HTTP %d", resp.StatusCode)
		}
	}
	var envelope struct {
		Code string
		Data json.RawMessage
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&envelope); err != nil {
		return errors.New("invalid PhotoLib response")
	}
	if envelope.Code != "OK" {
		return errors.New("unsuccessful PhotoLib response")
	}
	if output != nil {
		if err := json.Unmarshal(envelope.Data, output); err != nil {
			return errors.New("invalid PhotoLib response data")
		}
	}
	return nil
}

func (p photoLibClient) user(ctx context.Context, accessToken string) (photoLibUser, error) {
	var user photoLibUser
	if err := p.call(ctx, "/me", nil, accessToken, &user); err != nil {
		return photoLibUser{}, err
	}
	if user.ID <= 0 || user.Username == "" || user.MustChangePassword || user.MFA.EnrollmentRequired {
		return photoLibUser{}, errPhotoLibSession
	}
	return user, nil
}

func outboundClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
