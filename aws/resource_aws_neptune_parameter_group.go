package aws

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/hashicorp/terraform/helper/hashcode"
	"github.com/hashicorp/terraform/helper/resource"
	"github.com/hashicorp/terraform/helper/schema"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/neptune"
)

// We can only modify 20 parameters at a time, so walk them until
// we've got them all.
const maxParams = 20

func resourceAwsNeptuneParameterGroup() *schema.Resource {
	return &schema.Resource{
		Create: resourceAwsNeptuneParameterGroupCreate,
		Read:   resourceAwsNeptuneParameterGroupRead,
		Update: resourceAwsNeptuneParameterGroupUpdate,
		Delete: resourceAwsNeptuneParameterGroupDelete,
		Importer: &schema.ResourceImporter{
			State: schema.ImportStatePassthrough,
		},
		Schema: map[string]*schema.Schema{
			"name": {
				Type:     schema.TypeString,
				ForceNew: true,
				Required: true,
				StateFunc: func(val interface{}) string {
					return strings.ToLower(val.(string))
				},
			},
			"family": {
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
			},
			"description": {
				Type:     schema.TypeString,
				Optional: true,
				ForceNew: true,
				Default:  "Managed by Terraform",
			},
			"parameter": {
				Type:     schema.TypeSet,
				Optional: true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"name": {
							Type:     schema.TypeString,
							Required: true,
						},
						"value": {
							Type:     schema.TypeString,
							Required: true,
						},
						"apply_method": {
							Type:     schema.TypeString,
							Optional: true,
							Default:  "immediate",
							// this parameter is not actually state, but a
							// meta-parameter describing how the RDS API call
							// to modify the parameter group should be made.
							// Future reads of the resource from AWS don't tell
							// us what we used for apply_method previously, so
							// by squashing state to an empty string we avoid
							// needing to do an update for every future run.
							StateFunc: func(interface{}) string { return "" },
						},
					},
				},
				Set: resourceAwsNeptuneParameterHash,
			},
		},
	}
}

func resourceAwsNeptuneParameterGroupCreate(d *schema.ResourceData, meta interface{}) error {
	conn := meta.(*AWSClient).neptuneconn

	createOpts := neptune.CreateDBParameterGroupInput{
		DBParameterGroupName:   aws.String(d.Get("name").(string)),
		DBParameterGroupFamily: aws.String(d.Get("family").(string)),
		Description:            aws.String(d.Get("description").(string)),
	}

	log.Printf("[DEBUG] Create Neptune Parameter Group: %#v", createOpts)
	resp, err := conn.CreateDBParameterGroup(&createOpts)
	if err != nil {
		return fmt.Errorf("Error creating Neptune Parameter Group: %s", err)
	}

	d.Partial(true)
	d.SetPartial("name")
	d.SetPartial("family")
	d.SetPartial("description")
	d.Partial(false)

	d.SetId(*resp.DBParameterGroup.DBParameterGroupName)
	log.Printf("[INFO] Neptune Parameter Group ID: %s", d.Id())

	return resourceAwsNeptuneParameterGroupUpdate(d, meta)
}

func resourceAwsNeptuneParameterGroupRead(d *schema.ResourceData, meta interface{}) error {
	conn := meta.(*AWSClient).neptuneconn

	describeOpts := neptune.DescribeDBParameterGroupsInput{
		DBParameterGroupName: aws.String(d.Id()),
	}

	describeResp, err := conn.DescribeDBParameterGroups(&describeOpts)
	if err != nil {
		if isAWSErr(err, neptune.ErrCodeDBParameterGroupNotFoundFault, "") {
			log.Printf("[WARN] Neptune Parameter Group (%s) not found, removing from state", d.Id())
			d.SetId("")
			return nil
		}
		return err
	}

	if len(describeResp.DBParameterGroups) != 1 ||
		*describeResp.DBParameterGroups[0].DBParameterGroupName != d.Id() {
		return fmt.Errorf("Unable to find Parameter Group: %#v", describeResp.DBParameterGroups)
	}

	d.Set("name", describeResp.DBParameterGroups[0].DBParameterGroupName)
	d.Set("family", describeResp.DBParameterGroups[0].DBParameterGroupFamily)
	d.Set("description", describeResp.DBParameterGroups[0].Description)

	// Only include user customized parameters as there's hundreds of system/default ones
	describeParametersOpts := neptune.DescribeDBParametersInput{
		DBParameterGroupName: aws.String(d.Id()),
		Source:               aws.String("user"),
	}

	var parameters []*neptune.Parameter
	err = conn.DescribeDBParametersPages(&describeParametersOpts,
		func(describeParametersResp *neptune.DescribeDBParametersOutput, lastPage bool) bool {
			parameters = append(parameters, describeParametersResp.Parameters...)
			return !lastPage
		})
	if err != nil {
		return err
	}

	if err := d.Set("parameter", flattenNeptuneParameters(parameters)); err != nil {
		return fmt.Errorf("error setting parameter: %s", err)
	}

	return nil
}

func resourceAwsNeptuneParameterGroupUpdate(d *schema.ResourceData, meta interface{}) error {
	conn := meta.(*AWSClient).neptuneconn

	d.Partial(true)

	if d.HasChange("parameter") {
		o, n := d.GetChange("parameter")
		if o == nil {
			o = new(schema.Set)
		}
		if n == nil {
			n = new(schema.Set)
		}

		os := o.(*schema.Set)
		ns := n.(*schema.Set)

		toRemove, err := expandNeptuneParameters(os.Difference(ns).List())
		if err != nil {
			return err
		}

		log.Printf("[DEBUG] Parameters to remove: %#v", toRemove)

		toAdd, err := expandNeptuneParameters(ns.Difference(os).List())
		if err != nil {
			return err
		}

		log.Printf("[DEBUG] Parameters to add: %#v", toAdd)

		for len(toRemove) > 0 {
			paramsToModify := make([]*neptune.Parameter, 0)
			if len(toRemove) <= maxParams {
				paramsToModify, toRemove = toRemove[:], nil
			} else {
				paramsToModify, toRemove = toRemove[:maxParams], toRemove[maxParams:]
			}
			resetOpts := neptune.ResetDBParameterGroupInput{
				DBParameterGroupName: aws.String(d.Get("name").(string)),
				Parameters:           paramsToModify,
			}

			log.Printf("[DEBUG] Reset Neptune Parameter Group: %s", resetOpts)
			err := resource.Retry(30*time.Second, func() *resource.RetryError {
				_, err = conn.ResetDBParameterGroup(&resetOpts)
				if err != nil {
					if isAWSErr(err, "InvalidDBParameterGroupState", " has pending changes") {
						return resource.RetryableError(err)
					}
					return resource.NonRetryableError(err)
				}
				return nil
			})
			if err != nil {
				return fmt.Errorf("Error resetting Neptune Parameter Group: %s", err)
			}
		}

		for len(toAdd) > 0 {
			paramsToModify := make([]*neptune.Parameter, 0)
			if len(toAdd) <= maxParams {
				paramsToModify, toAdd = toAdd[:], nil
			} else {
				paramsToModify, toAdd = toAdd[:maxParams], toAdd[maxParams:]
			}
			modifyOpts := neptune.ModifyDBParameterGroupInput{
				DBParameterGroupName: aws.String(d.Get("name").(string)),
				Parameters:           paramsToModify,
			}

			log.Printf("[DEBUG] Modify Neptune Parameter Group: %s", modifyOpts)
			_, err = conn.ModifyDBParameterGroup(&modifyOpts)
			if err != nil {
				return fmt.Errorf("Error modifying Neptune Parameter Group: %s", err)
			}
		}

		d.SetPartial("parameter")
	}

	d.Partial(false)

	return resourceAwsNeptuneParameterGroupRead(d, meta)
}

func resourceAwsNeptuneParameterGroupDelete(d *schema.ResourceData, meta interface{}) error {
	conn := meta.(*AWSClient).neptuneconn

	return resource.Retry(3*time.Minute, func() *resource.RetryError {
		deleteOpts := neptune.DeleteDBParameterGroupInput{
			DBParameterGroupName: aws.String(d.Id()),
		}
		_, err := conn.DeleteDBParameterGroup(&deleteOpts)
		if err != nil {
			if isAWSErr(err, neptune.ErrCodeDBParameterGroupNotFoundFault, "") {
				return nil
			}
			if isAWSErr(err, neptune.ErrCodeInvalidDBParameterGroupStateFault, "") {
				return resource.RetryableError(err)
			}
			return resource.NonRetryableError(err)
		}
		return nil
	})
}

func resourceAwsNeptuneParameterHash(v interface{}) int {
	var buf bytes.Buffer
	m := v.(map[string]interface{})
	buf.WriteString(fmt.Sprintf("%s-", m["name"].(string)))
	// Store the value as a lower case string, to match how we store them in flattenParameters
	buf.WriteString(fmt.Sprintf("%s-", strings.ToLower(m["value"].(string))))

	return hashcode.String(buf.String())
}
