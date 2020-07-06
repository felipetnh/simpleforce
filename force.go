package simpleforce

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/pkg/errors"
)

const (
	DefaultAPIVersion = "43.0"
	DefaultClientID   = "simpleforce"
	DefaultURL        = "https://login.salesforce.com"

	logPrefix = "[simpleforce]"
)

var (
	// ErrFailure is a generic error if none of the other errors are appropriate.
	ErrFailure = errors.New("general failure")
	// ErrAuthentication is returned when authentication failed.
	ErrAuthentication = errors.New("authentication failure")
)

// Client is the main instance to access salesforce.
type Client struct {
	sessionID string
	user      struct {
		id       string
		name     string
		fullName string
		email    string
	}
	clientID      string
	apiVersion    string
	baseURL       string
	instanceURL   string
	useToolingAPI bool
	httpClient    *http.Client
}

// QueryResult holds the response data from an SOQL query.
type QueryResult struct {
	TotalSize      int       `json:"totalSize"`
	Done           bool      `json:"done"`
	NextRecordsURL string    `json:"nextRecordsUrl"`
	Records        []SObject `json:"records"`
}

// Query runs an SOQL query. q could either be the SOQL string or the nextRecordsURL.
func (client *Client) Query(q string) (*QueryResult, error) {
	if !client.isLoggedIn() {
		return nil, errors.New("Client is not logged in")
	}

	var u string
	if strings.HasPrefix(q, "/services/data") {
		// q is nextRecordsURL.
		u = fmt.Sprintf("%s%s", client.baseURL, q)
	} else {
		// q is SOQL.
		formatString := "%s/services/data/v%s/query?q=%s"
		baseURL := client.instanceURL
		if client.useToolingAPI {
			formatString = strings.Replace(formatString, "query", "tooling/query", -1)
		}
		u = fmt.Sprintf(formatString, baseURL, client.apiVersion, url.PathEscape(q))
	}

	data, err := client.httpRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}

	var result QueryResult
	err = json.Unmarshal(data, &result)
	if err != nil {
		return nil, err
	}

	// Reference to client is needed if the object will be further used to do online queries.
	for idx := range result.Records {
		result.Records[idx].setClient(client)
	}

	return &result, nil
}

// SObject creates an SObject instance with provided type name and associate the SObject with the client.
func (client *Client) SObject(typeName ...string) *SObject {
	obj := &SObject{}
	obj.setClient(client)
	if typeName != nil {
		obj.setType(typeName[0])
	}
	return obj
}

// isLoggedIn returns if the login to salesforce is successful.
func (client *Client) isLoggedIn() bool {
	return client.sessionID != ""
}

// LoginPassword signs into salesforce using password. token is optional if trusted IP is configured.
// Ref: https://developer.salesforce.com/docs/atlas.en-us.214.0.api_rest.meta/api_rest/intro_understanding_username_password_oauth_flow.htm
// Ref: https://developer.salesforce.com/docs/atlas.en-us.214.0.api.meta/api/sforce_api_calls_login.htm
func (client *Client) LoginPassword(username, password, token string) error {
	// Use the SOAP interface to acquire session ID with username, password, and token.
	// Do not use REST interface here as REST interface seems to have strong checking against client_id, while the SOAP
	// interface allows a non-exist placeholder client_id to be used.
	soapBody := `<?xml version="1.0" encoding="utf-8" ?>
        <env:Envelope
                xmlns:xsd="http://www.w3.org/2001/XMLSchema"
                xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
                xmlns:env="http://schemas.xmlsoap.org/soap/envelope/"
                xmlns:urn="urn:partner.soap.sforce.com">
            <env:Header>
                <urn:CallOptions>
                    <urn:client>%s</urn:client>
                    <urn:defaultNamespace>sf</urn:defaultNamespace>
                </urn:CallOptions>
            </env:Header>
            <env:Body>
                <n1:login xmlns:n1="urn:partner.soap.sforce.com">
                    <n1:username>%s</n1:username>
                    <n1:password>%s%s</n1:password>
                </n1:login>
            </env:Body>
        </env:Envelope>`
	soapBody = fmt.Sprintf(soapBody, client.clientID, username, html.EscapeString(password), token)

	url := fmt.Sprintf("%s/services/Soap/u/%s", client.baseURL, client.apiVersion)
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(soapBody))
	if err != nil {
		return err
	}
	req.Header.Add("Content-Type", "text/xml")
	req.Header.Add("charset", "UTF-8")
	req.Header.Add("SOAPAction", "login")

	resp, err := client.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return errors.New(
			fmt.Sprintf("Response status code error -- Status Code: %s", resp.StatusCode),
		)
	}

	respData, err := ioutil.ReadAll(resp.Body)

	if err != nil {
		return err
	}

	var loginResponse struct {
		XMLName      xml.Name `xml:"Envelope"`
		ServerURL    string   `xml:"Body>loginResponse>result>serverUrl"`
		SessionID    string   `xml:"Body>loginResponse>result>sessionId"`
		UserID       string   `xml:"Body>loginResponse>result>userId"`
		UserEmail    string   `xml:"Body>loginResponse>result>userInfo>userEmail"`
		UserFullName string   `xml:"Body>loginResponse>result>userInfo>userFullName"`
		UserName     string   `xml:"Body>loginResponse>result>userInfo>userName"`
	}

	err = xml.Unmarshal(respData, &loginResponse)
	if err != nil {
		return err
	}

	// Now we should all be good and the sessionID can be used to talk to salesforce further.
	client.sessionID = loginResponse.SessionID
	client.instanceURL, err = parseHost(loginResponse.ServerURL, client)
	if err != nil {
		return err
	}
	client.user.id = loginResponse.UserID
	client.user.name = loginResponse.UserName
	client.user.email = loginResponse.UserEmail
	client.user.fullName = loginResponse.UserFullName

	return nil
}

// httpRequest executes an HTTP request to the salesforce server and returns the response data in byte buffer.
func (client *Client) httpRequest(method, url string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}

	req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", client.sessionID))
	req.Header.Add("Content-Type", "application/json")

	resp, err := client.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		respErrorBody, e := ioutil.ReadAll(resp.Body)
		if e == nil {
			e = errors.New(
				fmt.Sprintf(
					"Response status code error -- Status: %s . With error: %s",
					resp.Status,
					string(respErrorBody),
				),
			)
			return nil, e
		}
		err = errors.New(
			fmt.Sprintf(
				"Response status code error -- Status: %s",
				resp.Status,
			),
		)
		return nil, err
	}

	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return respBody, nil
}

// makeURL generates a REST API URL based on baseURL, APIVersion of the client.
func (client *Client) makeURL(req string) string {
	retURL := fmt.Sprintf("%s/services/data/v%s/%s", client.instanceURL, client.apiVersion, req)
	// Fix potential problems
	retURL = strings.Replace(retURL, "vv", "v", -1)
	return retURL
}

// NewClient creates a new instance of the client.
func NewClient(url, clientID, apiVersion string) *Client {
	client := &Client{
		apiVersion: apiVersion,
		baseURL:    url,
		clientID:   clientID,
		httpClient: &http.Client{},
	}

	// Append "/" to the end of baseURL if not yet.
	if !strings.HasSuffix(client.baseURL, "/") {
		client.baseURL = client.baseURL + "/"
	}
	return client
}

// SetHTTPClient set a new HTTP client
func (client *Client) SetHTTPClient(c *http.Client) {
	client.httpClient = c
}

// DownloadFile downloads a file based on the REST API path given. Saves to filePath.
func (client *Client) DownloadFile(contentVersionID string, filepath string) error {

	apiPath := fmt.Sprintf("/services/data/v%s/sobjects/ContentVersion/%s/VersionData", client.apiVersion, contentVersionID)

	baseURL := strings.TrimRight(client.baseURL, "/")
	url := fmt.Sprintf("%s%s", baseURL, apiPath)

	// Get the data
	httpClient := client.httpClient
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}

	req.Header.Add("Content-Type", "application/json; charset=UTF-8")
	req.Header.Add("Accept", "application/json")
	req.Header.Add("Authorization", "Bearer "+client.sessionID)

	// resp, err := http.Get(url)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Create the file
	out, err := os.Create(filepath)
	if err != nil {
		return err
	}
	defer out.Close()

	// Write the body to file
	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return err
	}
	return nil
}

func parseHost(input string, client *Client) (string, error) {
	parsed, err := url.Parse(input)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host), nil
}
