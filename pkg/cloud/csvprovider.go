package cloud

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/opencost/opencost/pkg/env"
	"github.com/opencost/opencost/pkg/util"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/opencost/opencost/pkg/log"
	v1 "k8s.io/api/core/v1"

	"github.com/jszwec/csvutil"
)

const refreshMinutes = 60

type CSVProvider struct {
	*CustomProvider
	CSVLocation             string
	Pricing                 map[string]*price
	NodeClassPricing        map[string]float64
	NodeClassCount          map[string]float64
	NodeMapField            string
	PricingPV               map[string]*price
	PVMapField              string
	GPUClassPricing         map[string]*price
	GPUMapFields            []string // Fields in a node's labels that represent the GPU class.
	UsesRegion              bool
	DownloadPricingDataLock sync.RWMutex
}
type price struct {
	EndTimestamp      string `csv:"EndTimestamp"`
	InstanceID        string `csv:"InstanceID"`
	Region            string `csv:"Region"`
	AssetClass        string `csv:"AssetClass"`
	InstanceIDField   string `csv:"InstanceIDField"`
	InstanceType      string `csv:"InstanceType"`
	MarketPriceHourly string `csv:"MarketPriceHourly"`
	Version           string `csv:"Version"`
}

func GetCsv(location string) (io.Reader, error) {
	return os.Open(location)
}

func (c *CSVProvider) DownloadPricingData() error {
	c.DownloadPricingDataLock.Lock()
	defer time.AfterFunc(refreshMinutes*time.Minute, func() { c.DownloadPricingData() })
	defer c.DownloadPricingDataLock.Unlock()
	pricing := make(map[string]*price)
	nodeclasspricing := make(map[string]float64)
	nodeclasscount := make(map[string]float64)
	pvpricing := make(map[string]*price)
	gpupricing := make(map[string]*price)
	c.GPUMapFields = make([]string, 0, 1)
	header, err := csvutil.Header(price{}, "csv")
	if err != nil {
		return err
	}
	fieldsPerRecord := len(header)
	var csvr io.Reader
	var csverr error
	if strings.HasPrefix(c.CSVLocation, "s3://") {
		region := env.GetCSVRegion()
		conf := aws.NewConfig().WithRegion(region).WithCredentialsChainVerboseErrors(true)
		endpoint := env.GetCSVEndpoint()
		if endpoint != "" {
			conf = conf.WithEndpoint(endpoint)
		}
		s3Client := s3.New(session.New(conf))
		bucketAndKey := strings.Split(strings.TrimPrefix(c.CSVLocation, "s3://"), "/")
		if len(bucketAndKey) == 2 {
			out, err := s3Client.GetObject(&s3.GetObjectInput{
				Bucket: aws.String(bucketAndKey[0]),
				Key:    aws.String(bucketAndKey[1]),
			})
			csverr = err
			csvr = out.Body
		} else {
			c.Pricing = pricing
			c.NodeClassPricing = nodeclasspricing
			c.NodeClassCount = nodeclasscount
			c.PricingPV = pvpricing
			c.GPUClassPricing = gpupricing
			return fmt.Errorf("Invalid s3 URI: %s", c.CSVLocation)
		}
	} else {
		csvr, csverr = GetCsv(c.CSVLocation)
	}
	if csverr != nil {
		log.Infof("Error reading csv at %s: %s", c.CSVLocation, csverr)
		c.Pricing = pricing
		c.NodeClassPricing = nodeclasspricing
		c.NodeClassCount = nodeclasscount
		c.PricingPV = pvpricing
		c.GPUClassPricing = gpupricing
		return nil
	}
	csvReader := csv.NewReader(csvr)
	csvReader.Comma = ','
	csvReader.FieldsPerRecord = fieldsPerRecord

	dec, err := csvutil.NewDecoder(csvReader, header...)
	if err != nil {
		c.Pricing = pricing
		c.NodeClassPricing = nodeclasspricing
		c.NodeClassCount = nodeclasscount
		c.PricingPV = pvpricing
		c.GPUClassPricing = gpupricing
		return err
	}
	for {
		p := price{}
		err := dec.Decode(&p)
		csvParseErr, isCsvParseErr := err.(*csv.ParseError)
		if err == io.EOF {
			break
		} else if err == csvutil.ErrFieldCount || (isCsvParseErr && csvParseErr.Err == csv.ErrFieldCount) {
			rec := dec.Record()
			if len(rec) != 1 {
				log.Infof("Expected %d price info fields but received %d: %s", fieldsPerRecord, len(rec), rec)
				continue
			}
			if strings.Index(rec[0], "#") == 0 {
				continue
			} else {
				log.Infof("skipping non-CSV line: %s", rec)
				continue
			}
		} else if err != nil {
			log.Infof("Error during spot info decode: %+v", err)
			continue
		}
		log.Infof("Found price info %+v", p)
		key := strings.ToLower(p.InstanceID)
		if p.Region != "" { // strip the casing from region and add to key.
			key = fmt.Sprintf("%s,%s", strings.ToLower(p.Region), strings.ToLower(p.InstanceID))
			c.UsesRegion = true
		}
		if p.AssetClass == "pv" {
			pvpricing[key] = &p
			c.PVMapField = p.InstanceIDField
		} else if p.AssetClass == "node" {
			pricing[key] = &p
			classKey := p.Region + "," + p.InstanceType + "," + p.AssetClass
			cost, err := strconv.ParseFloat(p.MarketPriceHourly, 64)
			if err != nil {

			} else {
				if _, ok := nodeclasspricing[classKey]; ok {
					oldPrice := nodeclasspricing[classKey]
					oldCount := nodeclasscount[classKey]
					newPrice := ((oldPrice * oldCount) + cost) / (oldCount + 1.0)
					nodeclasscount[classKey] = newPrice
					nodeclasscount[classKey]++
				} else {
					nodeclasspricing[classKey] = cost
					nodeclasscount[classKey] = 1
				}
			}

			c.NodeMapField = p.InstanceIDField
		} else if p.AssetClass == "gpu" {
			gpupricing[key] = &p
			c.GPUMapFields = append(c.GPUMapFields, strings.ToLower(p.InstanceIDField))
		} else {
			log.Infof("Unrecognized asset class %s, defaulting to node", p.AssetClass)
			pricing[key] = &p
			c.NodeMapField = p.InstanceIDField
		}
	}
	if len(pricing) > 0 {
		c.Pricing = pricing
		c.NodeClassPricing = nodeclasspricing
		c.NodeClassCount = nodeclasscount
		c.PricingPV = pvpricing
		c.GPUClassPricing = gpupricing
	} else {
		log.DedupedWarningf(5, "No data received from csv at %s", c.CSVLocation)
	}
	return nil
}

type csvKey struct {
	Labels     map[string]string
	ProviderID string
	GPULabel   []string
	GPU        int64
}

func (k *csvKey) Features() string {
	instanceType, _ := util.GetInstanceType(k.Labels)
	region, _ := util.GetRegion(k.Labels)
	class := "node"

	return region + "," + instanceType + "," + class
}

func (k *csvKey) GPUCount() int {
	return int(k.GPU)
}

func (k *csvKey) GPUType() string {
	for _, label := range k.GPULabel {
		if val, ok := k.Labels[label]; ok {
			return val
		}
	}
	return ""
}
func (k *csvKey) ID() string {
	return k.ProviderID
}

func (c *CSVProvider) NodePricing(key Key) (*Node, error) {
	c.DownloadPricingDataLock.RLock()
	defer c.DownloadPricingDataLock.RUnlock()
	var node *Node
	if p, ok := c.Pricing[key.ID()]; ok {
		node = &Node{
			Cost:        p.MarketPriceHourly,
			PricingType: CsvExact,
		}
	}
	s := strings.Split(key.ID(), ",") // Try without a region to be sure
	if len(s) == 2 {
		if p, ok := c.Pricing[s[1]]; ok {
			node = &Node{
				Cost:        p.MarketPriceHourly,
				PricingType: CsvExact,
			}
		}
	}
	classKey := key.Features() // Use node attributes to try and do a class match
	if cost, ok := c.NodeClassPricing[classKey]; ok {
		log.Infof("Unable to find provider ID `%s`, using features:`%s`", key.ID(), key.Features())
		node = &Node{
			Cost:        fmt.Sprintf("%f", cost),
			PricingType: CsvClass,
		}
	}

	if node != nil {
		if t := key.GPUType(); t != "" {
			t = strings.ToLower(t)
			count := key.GPUCount()
			node.GPU = strconv.Itoa(count)
			hourly := 0.0
			if p, ok := c.GPUClassPricing[t]; ok {
				var err error
				hourly, err = strconv.ParseFloat(p.MarketPriceHourly, 64)
				if err != nil {
					log.Errorf("Unable to parse %s as float", p.MarketPriceHourly)
				}
			}
			totalCost := hourly * float64(count)
			node.GPUCost = fmt.Sprintf("%f", totalCost)
			nc, err := strconv.ParseFloat(node.Cost, 64)
			if err != nil {
				log.Errorf("Unable to parse %s as float", node.Cost)
			}
			node.Cost = fmt.Sprintf("%f", nc+totalCost)
		}
		return node, nil
	} else {
		return nil, fmt.Errorf("Unable to find Node matching `%s`:`%s`", key.ID(), key.Features())
	}
}

func NodeValueFromMapField(m string, n *v1.Node, useRegion bool) string {
	mf := strings.Split(m, ".")
	toReturn := ""
	if useRegion {
		if region, ok := util.GetRegion(n.Labels); ok {
			toReturn = region + ","
		} else {
			log.Errorf("Getting region based on labels failed")
		}
	}
	if len(mf) == 2 && mf[0] == "spec" && mf[1] == "providerID" {
		for matchNum, group := range provIdRx.FindStringSubmatch(n.Spec.ProviderID) {
			if matchNum == 2 {
				return toReturn + group
			}
		}
		if strings.HasPrefix(n.Spec.ProviderID, "azure://") {
			vmOrScaleSet := strings.ToLower(strings.TrimPrefix(n.Spec.ProviderID, "azure://"))
			return toReturn + vmOrScaleSet
		}
		return toReturn + n.Spec.ProviderID
	} else if len(mf) > 1 && mf[0] == "metadata" {
		if mf[1] == "name" {
			return toReturn + n.Name
		} else if mf[1] == "labels" {
			lkey := strings.Join(mf[2:len(mf)], "")
			return toReturn + n.Labels[lkey]
		} else if mf[1] == "annotations" {
			akey := strings.Join(mf[2:len(mf)], "")
			return toReturn + n.Annotations[akey]
		} else {
			log.Errorf("Unsupported InstanceIDField %s in CSV For Node", m)
			return ""
		}
	} else {
		log.Errorf("Unsupported InstanceIDField %s in CSV For Node", m)
		return ""
	}
}

func PVValueFromMapField(m string, n *v1.PersistentVolume) string {
	mf := strings.Split(m, ".")
	if len(mf) > 1 && mf[0] == "metadata" {
		if mf[1] == "name" {
			return n.Name
		} else if mf[1] == "labels" {
			lkey := strings.Join(mf[2:len(mf)], "")
			return n.Labels[lkey]
		} else if mf[1] == "annotations" {
			akey := strings.Join(mf[2:len(mf)], "")
			return n.Annotations[akey]
		} else {
			log.Errorf("Unsupported InstanceIDField %s in CSV For PV", m)
			return ""
		}
	} else if len(mf) > 2 && mf[0] == "spec" {
		if mf[1] == "capacity" && mf[2] == "storage" {
			skey := n.Spec.Capacity["storage"]
			return skey.String()
		} else {
			log.Infof("[ERROR] Unsupported InstanceIDField %s in CSV For PV", m)
			return ""
		}
	} else {
		log.Errorf("Unsupported InstanceIDField %s in CSV For PV", m)
		return ""
	}
}

func (c *CSVProvider) GetKey(l map[string]string, n *v1.Node) Key {
	id := NodeValueFromMapField(c.NodeMapField, n, c.UsesRegion)
	var gpuCount int64
	gpuCount = 0
	if gpuc, ok := n.Status.Capacity["nvidia.com/gpu"]; ok { // TODO: support non-nvidia GPUs
		gpuCount = gpuc.Value()
	}
	return &csvKey{
		ProviderID: id,
		Labels:     l,
		GPULabel:   c.GPUMapFields,
		GPU:        gpuCount,
	}
}

type csvPVKey struct {
	Labels                 map[string]string
	ProviderID             string
	StorageClassName       string
	StorageClassParameters map[string]string
	Name                   string
	DefaultRegion          string
}

func (key *csvPVKey) ID() string {
	return ""
}

func (key *csvPVKey) GetStorageClass() string {
	return key.StorageClassName
}

func (key *csvPVKey) Features() string {
	return key.ProviderID
}

func (c *CSVProvider) GetPVKey(pv *v1.PersistentVolume, parameters map[string]string, defaultRegion string) PVKey {
	id := PVValueFromMapField(c.PVMapField, pv)
	return &csvPVKey{
		Labels:                 pv.Labels,
		ProviderID:             id,
		StorageClassName:       pv.Spec.StorageClassName,
		StorageClassParameters: parameters,
		Name:                   pv.Name,
		DefaultRegion:          defaultRegion,
	}
}

func (c *CSVProvider) PVPricing(pvk PVKey) (*PV, error) {
	c.DownloadPricingDataLock.RLock()
	defer c.DownloadPricingDataLock.RUnlock()
	pricing, ok := c.PricingPV[pvk.Features()]
	if !ok {
		log.Infof("Persistent Volume pricing not found for %s: %s", pvk.GetStorageClass(), pvk.Features())
		return &PV{}, nil
	}
	return &PV{
		Cost: pricing.MarketPriceHourly,
	}, nil
}

func (c *CSVProvider) ServiceAccountStatus() *ServiceAccountStatus {
	return &ServiceAccountStatus{
		Checks: []*ServiceAccountCheck{},
	}
}

func (*CSVProvider) ClusterManagementPricing() (string, float64, error) {
	return "", 0.0, nil
}

func (c *CSVProvider) CombinedDiscountForNode(instanceType string, isPreemptible bool, defaultDiscount, negotiatedDiscount float64) float64 {
	return 1.0 - ((1.0 - defaultDiscount) * (1.0 - negotiatedDiscount))
}

func (c *CSVProvider) Regions() []string {
	return []string{}
}

func (c *CSVProvider) PricingSourceSummary() interface{} {
	return c.Pricing
}
