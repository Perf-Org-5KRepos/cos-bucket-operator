package bucket

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/IBM-Cloud/bluemix-go/models"
	ibmcloudv1alpha1 "github.com/ibm/cos-bucket-operator/pkg/apis/ibmcloud/v1alpha1"
)

const (
	authHeaderPrefix = "AWS4-HMAC-SHA256"
	timeFormat       = "20060102T150405Z"
	shortTimeFormat  = "20060102"
	signHeader       = "host;x-amz-date"

	// emptyStringSHA256 is a SHA256 of an empty string
	emptyStringSHA256 = `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`
)

func (r *ReconcileBucket) updateBucket(bucket *ibmcloudv1alpha1.Bucket, token string, serverInstanceID string, checkExistOnly bool) (bool, string, error) {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	var err error
	retnMessage := ""
	statusChange := false
	bucketName := bucket.GetObjectMeta().GetAnnotations()["BucketName"]
	urlPrefix, _ := getEndpointURL(bucket) // bucket.Spec.Resiliency, bucket.Spec.Location, bucket.Spec.BucketType)
	epString := fmt.Sprintf("https://%s/%s", urlPrefix, bucketName)
	log.Info("updateBucket", "ulrPrefix", urlPrefix, "epString", epString)
	log.Info("Call rest call ", "restAPI", epString)
	restClient := http.Client{
		Timeout:   time.Second * 300,
		Transport: tr,
	}
	// anno := bucket.ObjectMeta.GetAnnotations()
	instanceid := serverInstanceID // anno["ExternalInstanceID"]
	u, _ := url.ParseRequestURI(epString)
	urlStr := u.String()
	var out bytes.Buffer
	if bucket.Spec.StorageClass != "" {
		out = getStorageClassSpec(bucket)
	}
	method := "PUT"
	if checkExistOnly {
		method = "GET"
	}
	req, _ := http.NewRequest(method, urlStr, &out)

	// log.Info("", "out", out)
	req.Header.Set("Authorization", token)

	req.Header.Set("ibm-service-instance-id", instanceid)
	log.Info("Checking", "instanceid", instanceid)
	keyCRN := ""

	if bucket.Spec.KeyProtect != nil && bucket.GetObjectMeta().GetAnnotations()["KeyProtectKeyID"] == "" {
		_token, err := r.getIamToken(bucket.GetObjectMeta().GetNamespace(), bucket.Spec.KeyProtect.APIKey, nil)

		keyProtectInstanceID, err := r.readyKeyProtect(bucket.Spec.KeyProtect, bucket.GetObjectMeta().GetNamespace(), _token)
		if err != nil {
			return true, retnMessage, fmt.Errorf("%s, Please use IBmCloud APIKey", err)
		}
		if keyProtectInstanceID != "" {
			keyCRN, err = createKeyInKeyProtect(keyProtectInstanceID, bucket, _token)
			log.Info("Create KeyProtect", "Error", err)
			if err != nil {
				return true, retnMessage, err
			}
		}
		log.Info("createKeyInKeyProtect")
	}
	if keyCRN != "" {
		log.Info("Enable Key Protect", "CRN", keyCRN)
		req.Header.Set("ibm-sse-kp-customer-root-key-crn", keyCRN)
		req.Header.Set("ibm-sse-kp-encryption-algorithm", "AES256")
	}

	raw, err := restClient.Do(req)
	if err != nil {
		log.Info("Rest Call return ", "error", err)
		return statusChange, retnMessage, err
	}
	log.Info("Create Bucket return ", "StatusCode", raw.StatusCode)
	if raw.StatusCode != 200 {
		log.Info("Create Bucket return ", "error", raw.Body)
		if raw.StatusCode == 409 {
			// removeBucket(context.Background(), bucket, urlPrefix, token)
			return true, retnMessage, fmt.Errorf("This bucket name already exists in IBM Cloud Object Storage. Retrying")
		} else if raw.StatusCode == 404 && checkExistOnly {
			return true, retnMessage, fmt.Errorf("This bucket: %s does not exists", bucket.GetObjectMeta().GetAnnotations()["BucketName"])
		}
		return statusChange, retnMessage, fmt.Errorf("Create retnr %s", raw.Status)
	}
	if checkExistOnly {
		log.Info("Bucket exists", "name", bucket.GetObjectMeta().GetAnnotations()["BucketName"])
		updateAnnotation := false
		corsChanged := false
		retentionChanged := false
		if bucket.Spec.BindOnly {
			corsChanged, retentionChanged = r.checkBindOnlyDefault(bucket, serverInstanceID, token)
			log.Info("checkBindingDefault", "cors", corsChanged, "retention", retentionChanged)

		}
		if checkCORS(bucket) || corsChanged {
			log.Info("CorsRule had changed")
			accessCorsRule(bucket, instanceid, urlPrefix, token, "PUT", ibmcloudv1alpha1.CORSRule{})
			updateAnnotation = true
			statusChange = true
		}

		if checkRetentionPolicy(bucket) || retentionChanged {
			log.Info("Retention Policy had changed")

			_, retErr := accessRetentionPolicy(bucket, instanceid, urlPrefix, token, "PUT")
			if retErr != nil {
				log.Info("CreateRetention", "error", retErr)
				retnMessage = retErr.Error()
			} else {
				updateAnnotation = true
			}
			statusChange = true
		}
		if updateAnnotation {
			r.updateAnnotations(bucket, serverInstanceID)
		}
		return statusChange, retnMessage, nil
	}
	log.Info("Done creating", "bucketName is ", bucket.GetObjectMeta().GetAnnotations()["BucketName"])
	_, corsErr := accessCorsRule(bucket, instanceid, urlPrefix, token, "PUT", ibmcloudv1alpha1.CORSRule{})
	_, retErr := accessRetentionPolicy(bucket, instanceid, urlPrefix, token, "PUT")
	log.Info("in UpdateBucket", "corsErr", corsErr, "retErr", retErr)
	if corsErr != nil || retErr != nil {
		errMsg := ""
		if corsErr == nil {
			errMsg = retErr.Error()
		} else if retErr == nil {
			errMsg = corsErr.Error()
		} else {
			errMsg = corsErr.Error() + "; " + retErr.Error()
		}

		return statusChange, errMsg, nil
	}
	return statusChange, retnMessage, nil
}

type keyProtectBody struct {
	MetaData  metaDataStrut `json:"metadata"`
	Resources []keyResource `json:"resources"`
}
type metaDataStrut struct {
	CollectionType  string `json:"collectionType"`
	CollectionTotal int    `json:"collectionTotal"`
}

type keyResource struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Extractable bool   `json:"extractable"`
}
type keyProtectReturnBody struct {
	MetaData  metaDataStrut       `json:"metadata"`
	Resources []keyReturnResource `json:"resources"`
}
type keyReturnResource struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	State       string `json:"state"`
	CRN         string `json:"crn"`
	Extractable bool   `json:"extractable"`
	Imported    bool   `json:"imported"`
}

func createKeyInKeyProtect(keyProtectInstanceID string, bucket *ibmcloudv1alpha1.Bucket, token string) (string, error) {
	instanceLocation := bucket.Spec.KeyProtect.InstanceLocation
	keyName := bucket.Spec.KeyProtect.KeyName
	urlPrefix := getKeyProtectEndpoints(instanceLocation)
	epString := fmt.Sprintf("https://%s/api/v2/keys", urlPrefix)
	keyBody := keyProtectBody{
		MetaData: metaDataStrut{
			CollectionType:  "application/vnd.ibm.kms.key+json",
			CollectionTotal: 1,
		},
		Resources: []keyResource{
			keyResource{
				Type:        "application/vnd.ibm.kms.key+json",
				Name:        keyName,
				Description: "auto-generated by COS Operator",
				Extractable: false,
			},
		},
	}
	jsonBlob, _ := json.Marshal(keyBody)
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	restClient := http.Client{
		Timeout:   time.Second * 300,
		Transport: tr,
	}
	u, _ := url.ParseRequestURI(epString)
	urlStr := u.String()
	log.Info("KeyProtect", "Body", keyBody)
	req, _ := http.NewRequest("POST", urlStr, bytes.NewBuffer(jsonBlob))

	req.Header.Set("Authorization", token)
	req.Header.Set("bluemix-instance", keyProtectInstanceID)
	req.Header.Set("Content-Type", "application/json")
	res, err2 := restClient.Do(req)
	log.Info("KeyProtect", "returnStatus", res.StatusCode, "Error", err2)
	if res.StatusCode == 201 {
		body, _ := ioutil.ReadAll(res.Body)
		var keyReturn keyProtectReturnBody
		json.Unmarshal([]byte(body), &keyReturn)
		log.Info("CreateKey return", "body", body, "unmarshal", keyReturn)
		if len(keyReturn.Resources) == 0 {
			return "", nil
		}
		anno := bucket.GetObjectMeta().GetAnnotations()
		anno["KeyProtectKeyID"] = keyReturn.Resources[0].ID
		anno["KeyProtectInstanceID"] = keyProtectInstanceID
		bucket.GetObjectMeta().SetAnnotations(anno)
		return keyReturn.Resources[0].CRN, nil
	}
	return "", err2
}

func (r *ReconcileBucket) removeKeyInKeyProtect(bucket *ibmcloudv1alpha1.Bucket, token string) error {
	if bucket.Spec.KeyProtect != nil && bucket.Spec.KeyProtect.APIKey != nil {

		token, _ = r.getIamToken(bucket.GetObjectMeta().GetNamespace(), bucket.Spec.KeyProtect.APIKey, nil)
	}

	urlPrefix := getKeyProtectEndpoints(bucket.Spec.KeyProtect.InstanceLocation)
	resourceString := fmt.Sprintf("api/v2/keys/%s", bucket.GetObjectMeta().GetAnnotations()["KeyProtectKeyID"])
	epString := fmt.Sprintf("https://%s/%s", urlPrefix, resourceString)
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	restClient := http.Client{
		Timeout:   time.Second * 300,
		Transport: tr,
	}
	u, _ := url.ParseRequestURI(epString)
	urlStr := u.String()
	req, _ := http.NewRequest("DELETE", urlStr, nil)

	req.Header.Set("Authorization", token)

	req.Header.Set("bluemix-instance", bucket.GetObjectMeta().GetAnnotations()["KeyProtectInstanceID"])
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Prefer", "return=minimal")
	res, err2 := restClient.Do(req)
	log.Info("KeyProtect", "return", res, "Error", err2)
	if res.StatusCode < 400 {
		return nil
	}
	return err2
}

func accessCorsRule(bucket *ibmcloudv1alpha1.Bucket, instanceid string, urlPrefix string, token string, method string, _restoreCorsRule ibmcloudv1alpha1.CORSRule) (ibmcloudv1alpha1.CORSRule, error) {
	// Method: PUT https://<bucket endpoint>/<bucketname>?cors=ibmcloudv1alpha1

	inputRules := &_restoreCorsRule
	if reflect.DeepEqual(_restoreCorsRule, ibmcloudv1alpha1.CORSRule{}) {
		inputRules = &bucket.Spec.CORSRules
	}

	_corsRule := &ConfigCORSRule{AllowedHeader: inputRules.AllowedHeader, AllowedOrigin: inputRules.AllowedOrigin, AllowedMethod: inputRules.AllowedMethods}

	corsconfiguration := &CORSConfiguration{CORSRule: *_corsRule}
	log.Info("", "configuration", corsconfiguration)
	xmlBlob, _ := xml.Marshal(corsconfiguration)

	epString := fmt.Sprintf("https://%s/%s?cors=", urlPrefix, bucket.GetObjectMeta().GetAnnotations()["BucketName"])
	log.Info("", "epString", epString)
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	restClient := http.Client{
		Timeout:   time.Second * 300,
		Transport: tr,
	}
	u, _ := url.ParseRequestURI(epString)
	urlStr := u.String()
	req, _ := http.NewRequest(method, urlStr, bytes.NewBuffer(xmlBlob))
	req.Header.Set("Authorization", token)
	if method == "PUT" {
		os.Stdout.Write(xmlBlob)
		h := md5.New()
		xmlBlobStr := fmt.Sprintf("%s", xmlBlob)
		io.WriteString(h, xmlBlobStr)
		md5Str := base64.StdEncoding.EncodeToString(h.Sum(nil))
		log.Info("", "md5", md5Str)
		req.Header.Set("content-MD5", md5Str)

	}

	req.Header.Set("ibm-service-instance-id", instanceid)
	res, err2 := restClient.Do(req)
	if method == "PUT" {
		if err2 != nil {
			log.Info("Rest Call return ", "error", err2)
			return ibmcloudv1alpha1.CORSRule{}, err2
		}
		return ibmcloudv1alpha1.CORSRule{}, nil
	}
	if err2 == nil {
		currentCORS := ibmcloudv1alpha1.CORSRule{}
		if res.StatusCode == 404 {
			return currentCORS, nil
		}
		corsConfiguration := CORSConfiguration{}
		body, _ := ioutil.ReadAll(res.Body)

		xml.Unmarshal([]byte(body), &corsConfiguration)
		currentCORS.AllowedHeader = corsConfiguration.CORSRule.AllowedHeader
		currentCORS.AllowedMethods = corsConfiguration.CORSRule.AllowedMethod
		currentCORS.AllowedOrigin = corsConfiguration.CORSRule.AllowedOrigin
		return currentCORS, nil
	}
	return ibmcloudv1alpha1.CORSRule{}, err2
}

/* type retentionFailError struct {
	thisError Error `xml:"Error"`
}
*/
type Error struct {
	Code           string `xml:"Code"`
	Message        string `xml:"Message"`
	Resource       string `xml:"Resource"`
	RequestID      string `xml:"RequestId"`
	HTTPStatusCode int    `xml:"httpStatusCode"`
}

func accessRetentionPolicy(bucket *ibmcloudv1alpha1.Bucket, instanceid string, urlPrefix string, token string, method string) (ibmcloudv1alpha1.RetentionPolicy, error) {
	if (bucket.Spec.RetentionPolicy == ibmcloudv1alpha1.RetentionPolicy{}) {
		return ibmcloudv1alpha1.RetentionPolicy{}, nil
	}
	// Method: PUT https://<bucket endpoint>/<bucketname>?cors=
	retentionPolicy := &bucket.Spec.RetentionPolicy
	if retentionPolicy.DefaultRetentionDay > retentionPolicy.MaximumRetentionDay ||
		retentionPolicy.DefaultRetentionDay < retentionPolicy.MinimumRetentionDay {
		return ibmcloudv1alpha1.RetentionPolicy{}, fmt.Errorf("Default Retention value must be between Maximum and Minimum Retention")
	}

	_maxRetentionDay := &RetentionDay{Days: retentionPolicy.MaximumRetentionDay}
	_minRetentionDay := &RetentionDay{Days: retentionPolicy.MinimumRetentionDay}
	_defRetentionDay := &RetentionDay{Days: retentionPolicy.DefaultRetentionDay}
	protectionconfiguration := &ProtectionConfiguration{Status: "Retention", MinimumRetention: *_minRetentionDay, MaximumRetention: *_maxRetentionDay, DefaultRetention: *_defRetentionDay}
	log.Info("", "configuration", protectionconfiguration)
	xmlBlob, _ := xml.Marshal(protectionconfiguration)

	epString := fmt.Sprintf("https://%s/%s?protection=", urlPrefix, bucket.GetObjectMeta().GetAnnotations()["BucketName"])
	log.Info("", "epString", epString)
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	restClient := http.Client{
		Timeout:   time.Second * 300,
		Transport: tr,
	}
	u, _ := url.ParseRequestURI(epString)
	urlStr := u.String()
	req, _ := http.NewRequest(method, urlStr, bytes.NewBuffer(xmlBlob))
	req.Header.Set("Authorization", token)
	if method == "PUT" {
		os.Stdout.Write(xmlBlob)
		h := md5.New()
		xmlBlobStr := fmt.Sprintf("%s", xmlBlob)
		io.WriteString(h, xmlBlobStr)
		md5Str := base64.StdEncoding.EncodeToString(h.Sum(nil))
		log.Info("", "md5", md5Str)
		req.Header.Set("content-MD5", md5Str)

	}

	req.Header.Set("ibm-service-instance-id", instanceid)

	rst, err2 := restClient.Do(req)
	if method == "PUT" {
		if err2 != nil {
			log.Info("Rest Call return ", "error", err2)
			return ibmcloudv1alpha1.RetentionPolicy{}, err2
		}
		log.Info("Set Retention", "StatusCode", rst.StatusCode)
		if rst.StatusCode == 400 {
			_protectionFail := Error{}
			body, _ := ioutil.ReadAll(rst.Body)

			xml.Unmarshal([]byte(body), &_protectionFail)

			return ibmcloudv1alpha1.RetentionPolicy{}, fmt.Errorf("%s", _protectionFail.Message)
		}
		return ibmcloudv1alpha1.RetentionPolicy{}, nil
	}
	if err2 == nil {
		currentretPolicy := ibmcloudv1alpha1.RetentionPolicy{}
		if rst.StatusCode == 404 {
			return currentretPolicy, nil
		}
		protectionConfiguration := ProtectionConfiguration{}
		body, _ := ioutil.ReadAll(rst.Body)

		xml.Unmarshal([]byte(body), &protectionConfiguration)
		currentretPolicy.MinimumRetentionDay = protectionConfiguration.MinimumRetention.Days
		currentretPolicy.MaximumRetentionDay = protectionConfiguration.MaximumRetention.Days
		currentretPolicy.DefaultRetentionDay = protectionConfiguration.DefaultRetention.Days
		return currentretPolicy, nil
	}
	return ibmcloudv1alpha1.RetentionPolicy{}, nil
}

func removeBucket(context context.Context, bucket *ibmcloudv1alpha1.Bucket, urlPrefix string, token string) error {

	epString := fmt.Sprintf("https://%s/%s", urlPrefix, bucket.GetObjectMeta().GetAnnotations()["BucketName"])
	log.Info("Remove Bucket", "epString", epString)
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	restClient := http.Client{
		Timeout:   time.Second * 120,
		Transport: tr,
	}
	u, _ := url.ParseRequestURI(epString)
	urlStr := u.String()
	req, _ := http.NewRequest("DELETE", urlStr, nil)

	req.Header.Set("Authorization", token)
	req.Header.Set("Host", urlPrefix)
	raw, err := restClient.Do(req)

	if err != nil {
		log.Info("Delete Bucket failed %s", "error", err)
		return err
	}
	log.Info("Remove Bucket", "StatueCode", raw.StatusCode)
	if raw.StatusCode > 300 && raw.StatusCode != 404 && raw.StatusCode != 403 && raw.StatusCode != 405 {

		return fmt.Errorf("Remove Bucket failed, Statue Code = %d", raw.StatusCode)
	}
	log.Info("restcall return null")
	return nil
}

func removeObjectsInBucket(ctx context.Context, bucket *ibmcloudv1alpha1.Bucket, urlPrefix string, token string, deleteObjects Delete) error {
	epString := fmt.Sprintf("https://%s/%s?delete=", urlPrefix, bucket.GetObjectMeta().GetAnnotations()["BucketName"])

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	restClient := http.Client{
		Timeout:   time.Second * 300,
		Transport: tr,
	}
	u, _ := url.ParseRequestURI(epString)
	urlStr := u.String()
	xmlBlob, _ := xml.Marshal(&deleteObjects)
	req, _ := http.NewRequest("POST", urlStr, bytes.NewBuffer(xmlBlob))
	req.Header.Set("Authorization", token)

	h := md5.New()
	xmlBlobStr := fmt.Sprintf("%s", xmlBlob)
	io.WriteString(h, xmlBlobStr)
	md5Str := base64.StdEncoding.EncodeToString(h.Sum(nil))

	req.Header.Set("Content-MD5", md5Str)
	_, err := restClient.Do(req)
	if err != nil {
		log.Info("restCall return ", "error", err)
		return err
	}
	return nil
}
func locateObjectsInBucket(ctx context.Context, bucket *ibmcloudv1alpha1.Bucket, urlPrefix string, token string) Delete {
	delete := Delete{}
	epString := fmt.Sprintf("https://%s/%s", urlPrefix, bucket.GetObjectMeta().GetAnnotations()["BucketName"])
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	restClient := http.Client{
		Timeout:   time.Second * 300,
		Transport: tr,
	}
	u, _ := url.ParseRequestURI(epString)
	urlStr := u.String()

	req, _ := http.NewRequest("GET", urlStr, nil)

	req.Header.Set("Authorization", token)

	res, err2 := restClient.Do(req)
	if err2 != nil {
		log.Info(bucket.GetObjectMeta().GetAnnotations()["BucketName"], "error ", err2)
		return delete
	}
	body, _ := ioutil.ReadAll(res.Body)
	bucketInfo := ListBucketResult{}
	xml.Unmarshal([]byte(body), &bucketInfo)
	for _, obj := range bucketInfo.Contents {
		objKey := obj.Key
		delete.Object = append(delete.Object, Object{Key: objKey})
	}
	// xmlBlob, _ := xml.Marshal(&delete)
	log.Info(bucket.GetObjectMeta().GetAnnotations()["BucketName"], "Returning to be Deleted objs", delete)
	return delete
}

func getEndpointInfo(bucket *ibmcloudv1alpha1.Bucket, epString string, token string) Endpoints {
	endpoints := Endpoints{}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	restClient := http.Client{
		Timeout:   time.Second * 300,
		Transport: tr,
	}
	epString = strings.TrimSpace(epString)
	u, err := url.ParseRequestURI(epString)
	if err != nil {
		return endpoints
	}
	urlStr := u.String()

	req, _ := http.NewRequest("GET", urlStr, nil)

	req.Header.Set("Authorization", token)

	req.Header.Set("Content-Type", "application/json")
	res, err2 := restClient.Do(req)
	if err2 != nil {
		log.Info(bucket.GetObjectMeta().GetName(), "error ", err2)
		return endpoints
	}
	body, _ := ioutil.ReadAll(res.Body)
	err = json.Unmarshal([]byte(body), &endpoints)
	if err != nil {
		log.Info("Cannot Unmshal", "epString", epString, "body", body, "error", err)
		return Endpoints{}
	}

	// log.Info(bucket.GetObjectMeta().GetName(), "Endpoints", endpoints)
	return endpoints
}

func validInstance(instanceName string, instanceID string, token string) (string, error) {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	restClient := http.Client{
		Timeout:   time.Second * 300,
		Transport: tr,
	}

	u, err := url.ParseRequestURI("https://resource-controller.cloud.ibm.com/v2/resource_instances?limit=200")

	urlStr := u.String()

	req, _ := http.NewRequest("GET", urlStr, nil)

	req.Header.Set("Authorization", token)

	req.Header.Set("Content-Type", "application/json")
	res, err := restClient.Do(req)
	if err != nil {
		log.Info("validInstanec", "error ", err)
		return "", err
	}
	body, _ := ioutil.ReadAll(res.Body)

	var paginatedResources = struct {
		NextURL        string          `json:"next_url"`
		ResourcesBytes json.RawMessage `json:"resources"`
	}{}
	err = json.Unmarshal([]byte(body), &paginatedResources)
	if err != nil {
		log.Info("Cannot Unmshal", "body", body, "error", err)
		return "", err
	}
	var instances []models.ServiceInstance
	err = json.Unmarshal([]byte(paginatedResources.ResourcesBytes), &instances)

	for _, instance := range instances {

		_thisInstanceID := getGUID(instance.ID)
		if instance.Name == instanceName || _thisInstanceID == instanceID {
			return _thisInstanceID, nil
		}
	}
	return "", fmt.Errorf("No such instance found")
}
func ondemandAuthenticate(apiKeyVal string, region string) (string, error) {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	restClient := http.Client{
		Timeout:   time.Second * 300,
		Transport: tr,
	}

	epString := fmt.Sprintf("https://iam.cloud.ibm.com/identity/token?grant_type=urn:ibm:params:oauth:grant-type:apikey&apikey=%s", apiKeyVal)
	u, _ := url.ParseRequestURI(epString)
	urlStr := u.String()

	/* data := url.Values{}
	data.Set("grant_type", "urn:ibm:params:oauth:grant-type:apikey")
	data.Add("apikey", apiKeyVal) */
	req, _ := http.NewRequest("POST", urlStr, nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := restClient.Do(req)
	log.Info("Check return", "error", err)
	if err != nil || (res != nil && res.StatusCode != 200) {
		log.Info("GetToken", "error ", err, "statusCode", res.StatusCode)
		return "", err
	}

	body, _ := ioutil.ReadAll(res.Body)
	var tokenInfo = struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiredIn    int    `json:"expire_in"`
		Expiration   int    `json:"expiration"`
		Scope        string `json:"scope"`
	}{}
	err = json.Unmarshal([]byte(body), &tokenInfo)
	if err != nil {
		log.Info("Cannot Unmshal", "body", body, "error", err)
		return "", err
	}
	return tokenInfo.TokenType + " " + tokenInfo.AccessToken, nil
}

func getKeyProtectEndpoints(location string) string {
	if location == "" {
		location = "Dallas"
	}
	switch strings.ToLower(location) {
	case "dallas", "us-south":
		return "us-south.kms.cloud.ibm.com"
	case "washington DC", "us-east":
		return "us-east.kms.cloud.ibm.com"
	case "london", "eu-gb":
		return "eu-gb.kms.cloud.ibm.com"
	case "frankfurt", "eu-de":
		return "eu-de.kms.cloud.ibm.com"
	case "sydney", "au-syd":
		return "au-syd.kms.cloud.ibm.com"
	case "tokyo", "jp-tok":
		return "jp-tok.kms.cloud.ibm.com"
	}
	return "us-south.kms.cloud.ibm.com"
}

func makeHmac(key []byte, data []byte) []byte {
	hash := hmac.New(sha256.New, key)
	hash.Write(data)
	return hash.Sum(nil)
}
