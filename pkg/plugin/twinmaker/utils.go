package twinmaker

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"text/template"

	"github.com/aws/aws-sdk-go/service/iottwinmaker"
	"github.com/grafana/grafana-iot-twinmaker-app/pkg/models"
	"github.com/grafana/grafana-plugin-sdk-go/data"
)

type PolicyStatement struct {
	Effect    string   `json:"Effect"`
	Action    []string `json:"Action"`
	Resource  []string `json:"Resource"`
	Condition string   `json:"Condition,omitempty"`
}

type IAMPolicy struct {
	Version   string            `json:"Version"`
	Statement []PolicyStatement `json:"Statement"`
}

func LoadPolicy(workspace *iottwinmaker.GetWorkspaceOutput) (string, error) {
	data := map[string]interface{}{
		"S3BucketArn":  workspace.S3Location,
		"WorkspaceArn": workspace.Arn,
		"WorkspaceId":  workspace.WorkspaceId,
	}

	policyTemplate := `{
		"Version": "2012-10-17",
		"Statement": [
			{
				"Action": [
					"iottwinmaker:ListWorkspaces"
				],
				"Resource": [
					"*"
				],
				"Effect": "Allow"
			},
			{
				"Action": [
					"iottwinmaker:Get*",
					"iottwinmaker:List*"
				],
				"Resource": [
					"{{.WorkspaceArn}}",
					"{{.WorkspaceArn}}/*"
				],
				"Effect": "Allow"
			},
			{
				"Effect": "Allow",
				"Action": [
				  "kinesisvideo:GetDataEndpoint",
				  "kinesisvideo:GetHLSStreamingSessionURL"
				],
				"Resource": "*"
			},
			{
				"Effect": "Allow",
				"Action": [
				  "iotsitewise:GetAssetPropertyValue",
				  "iotsitewise:GetInterpolatedAssetPropertyValues"
				],
				"Resource": "*"
			},
			{
				 "Effect": "Allow",
				 "Action": [
				  "iotsitewise:BatchPutAssetPropertyValue"
				],
				"Resource": "*",
				"Condition": {
				  "StringLike": {
					"aws:ResourceTag/EdgeConnectorForKVS": "*{{.WorkspaceId}}*"
				  } 
				}
			},
			{
				"Effect": "Allow",
				"Action": ["s3:GetObject"],
				"Resource": [
					"{{.S3BucketArn}}", 
					"{{.S3BucketArn}}/*"
				]
			}
		]
	}`

	buffer := new(bytes.Buffer)
	err := json.Compact(buffer, []byte(policyTemplate))
	if err != nil {
		return "", err
	}
	policyTemplate = buffer.String()

	t := template.Must(template.New("policy").Parse(policyTemplate))
	builder := &strings.Builder{}

	err = t.Execute(builder, data)
	if err != nil {
		return "", err
	}

	return builder.String(), err
}

func checkForUrl(v *iottwinmaker.DataValue, convertor func(v *iottwinmaker.DataValue) interface{}) bool {
	val := convertor(v)
	switch val.(type) {
	case *string:
		val = *v.StringValue
		if strings.Contains(val.(string), "://") {
			return true
		}
	default:
		break
	}
	return false
}

func setUrlDatalink(field *data.Field) {
	field.Config = &data.FieldConfig{
		Links: []data.DataLink{
			{Title: "Link", URL: "${__value.text}", TargetBlank: true},
		},
	}
}

func GetEntityPropertyReferenceKey(entityPropertyReference *iottwinmaker.EntityPropertyReference) (s string) {
	externalId := ""
	for _, val := range entityPropertyReference.ExternalIdProperty {
		// Only one externalId in the mapping
		externalId = *val
		break
	}
	// Key is the combination of the unique entityId_componentName_externalId_propertyId
	refKey := ""
	if entityPropertyReference.EntityId != nil {
		refKey = refKey + *entityPropertyReference.EntityId + "_"
	}
	if entityPropertyReference.ComponentName != nil {
		refKey = refKey + *entityPropertyReference.ComponentName + "_"
	}
	refKey = refKey + externalId + "_"
	if entityPropertyReference.PropertyName != nil {
		refKey = refKey + *entityPropertyReference.PropertyName
	}
	return refKey
}

type PropertyReference struct {
	values     					[]*iottwinmaker.PropertyValue
	entityPropertyReference   	*iottwinmaker.EntityPropertyReference
	entityName		*string
}

func (s *twinMakerHandler) GetComponentHistoryWithLookup(ctx context.Context, query models.TwinMakerQuery) (p []PropertyReference, n []data.Notice, err error) {
	propertyReferences := []PropertyReference{}
	failures := []data.Notice{}
	componentTypeId := query.ComponentTypeId

	// Step 1: Call GetPropertyValueHistory and get the externalId from the response
	result, err := s.client.GetPropertyValueHistory(ctx, query)
	if err != nil {
		return propertyReferences, failures, err
	}

	if len(result.PropertyValues) > 0 {
		// Loop through all propertyValues if there are multiple components of the same type on the entity
		for _, propertyValue := range result.PropertyValues {
			externalId := ""
			for _, val := range propertyValue.EntityPropertyReference.ExternalIdProperty {
				// Only one externalId per component
				externalId = *val
				break
			}

			// Step 2: Call ListEntities with a filter for the externalId
			query.EntityId = ""
			query.Properties = nil
			query.ComponentTypeId = ""

			query.ListEntitiesFilter = []models.TwinMakerListEntitiesFilter{
				{
					ExternalId: externalId,
				},
			}
			le, err := s.client.ListEntities(ctx, query)
	
			if err != nil {
				notice := data.Notice{
					Severity: data.NoticeSeverityWarning,
					Text:     err.Error(),
				}
				failures = append(failures, notice)
			}
	
			// Step 3: Call GetEntity to get the componentName of the externalId
			if len(le.EntitySummaries) > 0 {
				entityId := le.EntitySummaries[0].EntityId
				entityName := le.EntitySummaries[0].EntityName
				query.EntityId = *entityId
				e, err := s.client.GetEntity(ctx, query)
				if err != nil {
					notice := data.Notice{
						Severity: data.NoticeSeverityWarning,
						Text:     err.Error(),
					}
					failures = append(failures, notice)
				}
				componentName := ""
				for _, component := range e.Components {
					// If the componentTypeId and externalId match then we found the component
					if *component.ComponentTypeId == componentTypeId {
						for _, property := range component.Properties {
							if *property.Definition.IsExternalId {
								if *property.Value.StringValue == externalId {
									componentName = *component.ComponentName
									break
								}
							}
						}
						break
					}
				}

				pr := PropertyReference{
					values: propertyValue.Values,
					entityPropertyReference: &iottwinmaker.EntityPropertyReference{
						EntityId: entityId,
						ComponentName: &componentName,
						ExternalIdProperty: propertyValue.EntityPropertyReference.ExternalIdProperty,
						PropertyName: propertyValue.EntityPropertyReference.PropertyName,
					},
					entityName: entityName,
				}
				propertyReferences = append(propertyReferences, pr)
			}
		}
	}

	return propertyReferences, failures, nil
}
