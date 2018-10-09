package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gomodule/redigo/redis"
)

// msisdn_sanitizer converts an msisdn to the international format
// with a + preceeding it dependent on if plus is true, else the plus is removed.
func MsisdnSanitizer(msisdn string, plus bool) string {
	// Remove whitespace
	sanitizedMsisdn := strings.TrimSpace(msisdn)
	// Remove all occurences of + in the MSISDN
	sanitizedMsisdn = strings.Replace(sanitizedMsisdn, "+", "", -1)
	if strings.HasPrefix(sanitizedMsisdn, "234") == true {
		sanitizedMsisdn = fmt.Sprintf("%s%s", "0", sanitizedMsisdn[3:len(sanitizedMsisdn)])
	}
	if len(sanitizedMsisdn) == 11 {
		sanitizedMsisdn = fmt.Sprintf("%s%s", "+234", sanitizedMsisdn[1:len(sanitizedMsisdn)])
	}
	if plus == false {
		sanitizedMsisdn = sanitizedMsisdn[1:len(sanitizedMsisdn)]
	}
	return sanitizedMsisdn

}

//This is to check if a susbscriber has been blacklisted
func IsMsisdnBlacklisted(msisdn string) bool {
	mno := "mtn"
	blacklistURL := os.Getenv("BLACKLIST_URL")
	msisdn = MsisdnSanitizer(msisdn, false)
	url := blacklistURL + "/search/" + mno + "?msisdn=" + msisdn
	log.Println("blacklist URL is", url)
	var netClient = &http.Client{
		Timeout: time.Second * 10,
	}
	response, err := netClient.Get(url)
	if err != nil {
		panic(err)
	}
	body, err := ioutil.ReadAll(response.Body)
	bodyData := string(body)
	data := make(map[string]interface{})
	err = json.Unmarshal([]byte(body), &data)
	if err != nil {
		panic(err)
	}
	log.Println(bodyData)
	return data["data"].(bool)
}

func generateMsisdnContentTrackKey(msisdn string, shortcode string) string {
	return "mtnadcontentcount:" + msisdn + "-" + shortcode
}

func generateCampignContentTrackKey(adID string) string {
	return "campaigncontentcount:" + adID
}

func isMonthlyRateExceeded(msisdn string, shortcode string, pool *redis.Pool) bool {
	msisdnKey := generateMsisdnContentTrackKey(msisdn, shortcode)
	log.Println("msisdnKey", msisdnKey)
	conn := pool.Get()
	defer conn.Close()

	var (
		count int
	)
	monthCount, err := redis.Int(conn.Do("GET", msisdnKey))
	if err != nil {
		log.Printf("error getting key %s: %v", msisdn, err)
		// panic(err)
	}
	if monthCount == 0 {
		count = 0
	} else {
		count = monthCount
	}
	monthlyContentCount, err := strconv.Atoi(os.Getenv("MONTHLY_CONTENT_COUNT"))
	if err != nil {
		panic(err)
	}
	return count >= monthlyContentCount
}

func isContentSentOnCampaign(msisdn string, adID string, pool *redis.Pool) bool {
	campaignKey := generateCampignContentTrackKey(adID)
	conn := pool.Get()
	defer conn.Close()

	value, err := redis.Bool(conn.Do("SISMEMEBER", campaignKey, msisdn))
	if err != nil {
		log.Println(err)
	}
	return value
}

func sendContent(param map[string]interface{}, flash bool) []byte {
	dlrURL := os.Getenv("DLR_URL")
	// shortcode, err := url.Parse(param["shortcode"].(string))
	// text, err := url.Parse(param["text"].(string))
	urlString := dlrURL + "/dlr/sms?" + "ad_id=" + param["ad_id"].(string) + "&zone_id=" + param["zone_id"].(string) + "&shortcode=" + param["shortcode"].(string) + "&text=" + param["text"].(string) + "&state=%d&msisdn=%p&report=%A&ts=%T"
	fullURL := os.Getenv("sms") + "/cgi-bin/sendsms?username=adrenaline&password=adrenaline&smsc=MTN_SEND&from=" + param["shortcode"].(string) + "&to=" + param["to"].(string) + "&text=" + param["text"].(string) + "&dlr-mask=24&dlr-url=" + urlString

	if flash == true {
		fullURL += "&mclass=0"
	}
	urlEncoded, err := url.Parse(fullURL)
	log.Println("send content URL", urlEncoded)

	resp, err := http.Get(urlEncoded.String())
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	return body
}

func trackContentSentToMsisdn(msisdn string, adID string, shortcode string) {
	msisdnKey := generateMsisdnContentTrackKey(msisdn, shortcode)
	monthTimeStamp := getCurrentMonthLastTimestamp()
	campaignHours, err := strconv.Atoi(os.Getenv("CAMPAIGN_ALLOWED_REPEAT_HOURS"))
	if err != nil {
		log.Println(err)
	}
	campaignExpiryTimestamp := getCurrentTimestampPlusXHours(campaignHours)
	campaignKey := generateCampignContentTrackKey(adID)
	addToSetWithExpiry(redisPool, campaignKey, msisdn, campaignExpiryTimestamp)
	increaseAndExpire(msisdnKey, monthTimeStamp, redisPool)
}

func addToSetWithExpiry(pool *redis.Pool, campaignKey string, msisdn string, campaignExpiryTimestamp int64) bool {
	conn := pool.Get()
	defer conn.Close()
	value, err := redis.Bool(conn.Do("SADD", campaignKey, msisdn))
	if err != nil {
		panic(err)
	}
	expireKeyAt(campaignKey, campaignExpiryTimestamp, redisPool)
	return value
}

func expireKeyAt(key string, expiry int64, pool *redis.Pool) bool {
	conn := pool.Get()
	defer conn.Close()
	value, err := redis.Bool(conn.Do("EXPIREAT", key, expiry))
	if err != nil {
		panic(err)
	}
	return value
}

func increaseAndExpire(campaignKey string, campaignExpiryTimestamp int64, pool *redis.Pool) bool {
	/* Increase the count of a key in redis by 1 and set the expiry of the key

	   Arguments:
	       key {string} -- Key to increase value
	       expiry {int} -- Expiry time of key for EXPIREAT

	   Returns:
	       [Array] -- Pipeline execition result in the form [int, boolean]
	       Where the int i the current value count and the boolean is the status of the EXPIRE
	*/
	conn := pool.Get()
	defer conn.Close()
	redis.Bool(conn.Do("MULTI"))
	redis.Bool(conn.Do("INCRBY", campaignKey))
	redis.Bool(conn.Do("EXPIREAT", campaignKey, campaignExpiryTimestamp))
	pipe, err := redis.Bool(conn.Do("EXEC"))
	if err != nil {
		panic(err)
	}
	return pipe

}

func getCurrentMonthLastTimestamp() int64 {
	year, month, _ := time.Now().Date()
	days := time.Date(0, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
	t := time.Date(year, month, days, 23, 59, 59, 999999, time.Local)
	return t.Unix()
}

func getCurrentTimestampPlusXHours(hours int) int64 {
	dt := time.Now().Add(time.Hour * time.Duration(hours))
	return dt.Unix()
}
