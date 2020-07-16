/*
Copyright 2020 Cortex Labs, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package spec

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/cortexlabs/cortex/pkg/consts"
	"github.com/cortexlabs/cortex/pkg/lib/aws"
	"github.com/cortexlabs/cortex/pkg/lib/cast"
	cr "github.com/cortexlabs/cortex/pkg/lib/configreader"
	"github.com/cortexlabs/cortex/pkg/lib/docker"
	"github.com/cortexlabs/cortex/pkg/lib/errors"
	"github.com/cortexlabs/cortex/pkg/lib/files"
	"github.com/cortexlabs/cortex/pkg/lib/k8s"
	libmath "github.com/cortexlabs/cortex/pkg/lib/math"
	"github.com/cortexlabs/cortex/pkg/lib/pointer"
	"github.com/cortexlabs/cortex/pkg/lib/regex"
	"github.com/cortexlabs/cortex/pkg/lib/sets/strset"
	s "github.com/cortexlabs/cortex/pkg/lib/strings"
	libtime "github.com/cortexlabs/cortex/pkg/lib/time"
	"github.com/cortexlabs/cortex/pkg/lib/urls"
	"github.com/cortexlabs/cortex/pkg/types"
	"github.com/cortexlabs/cortex/pkg/types/userconfig"
	kresource "k8s.io/apimachinery/pkg/api/resource"
)

var AutoscalingTickInterval = 10 * time.Second

func apiValidation(provider types.ProviderType, kind userconfig.Kind) *cr.StructValidation {
	structFieldValidations := []*cr.StructFieldValidation{}
	structFieldValidations = append(resourceStructValidations,
		predictorValidation(),
		monitoringValidation(),
		networkingValidation(),
		computeValidation(provider),
		autoscalingValidation(provider),
		updateStrategyValidation(provider),
	)
	return &cr.StructValidation{
		StructFieldValidations: structFieldValidations,
	}
}

var resourceStructValidations = []*cr.StructFieldValidation{
	{
		StructField: "Name",
		StringValidation: &cr.StringValidation{
			Required:  true,
			DNS1035:   true,
			MaxLength: 42, // k8s adds 21 characters to the pod name, and 63 is the max before it starts to truncate
		},
	},
	{
		StructField: "Kind",
		StringValidation: &cr.StringValidation{
			Required:      true,
			AllowedValues: userconfig.KindStrings(),
		},
		Parser: func(str string) (interface{}, error) {
			return userconfig.KindFromString(str), nil
		},
	},
}

func predictorValidation() *cr.StructFieldValidation {
	return &cr.StructFieldValidation{
		StructField: "Predictor",
		StructValidation: &cr.StructValidation{
			Required: true,
			StructFieldValidations: []*cr.StructFieldValidation{
				{
					StructField: "Type",
					StringValidation: &cr.StringValidation{
						Required:      true,
						AllowedValues: userconfig.PredictorTypeStrings(),
					},
					Parser: func(str string) (interface{}, error) {
						return userconfig.PredictorTypeFromString(str), nil
					},
				},
				{
					StructField: "Path",
					StringValidation: &cr.StringValidation{
						Required: true,
					},
				},
				{
					StructField: "ModelPath",
					StringPtrValidation: &cr.StringPtrValidation{
						Required: false,
					},
				},
				{
					StructField: "PythonPath",
					StringPtrValidation: &cr.StringPtrValidation{
						AllowEmpty:       false,
						DisallowedValues: []string{".", "./", "./."},
						Validator: func(path string) (string, error) {
							if files.IsAbsOrTildePrefixed(path) {
								return "", ErrorMustBeRelativeProjectPath(path)
							}
							path = strings.TrimPrefix(path, "./")
							path = s.EnsureSuffix(path, "/")
							return path, nil
						},
					},
				},
				{
					StructField: "Image",
					StringValidation: &cr.StringValidation{
						Required:           false,
						AllowEmpty:         true,
						DockerImageOrEmpty: true,
					},
				},
				{
					StructField: "TensorFlowServingImage",
					StringValidation: &cr.StringValidation{
						Required:           false,
						AllowEmpty:         true,
						DockerImageOrEmpty: true,
					},
				},
				{
					StructField: "ProcessesPerReplica",
					Int32Validation: &cr.Int32Validation{
						Default:              1,
						GreaterThanOrEqualTo: pointer.Int32(1),
						LessThanOrEqualTo:    pointer.Int32(100),
					},
				},
				{
					StructField: "ThreadsPerProcess",
					Int32Validation: &cr.Int32Validation{
						Default:              1,
						GreaterThanOrEqualTo: pointer.Int32(1),
					},
				},
				{
					StructField: "Config",
					InterfaceMapValidation: &cr.InterfaceMapValidation{
						StringKeysOnly:     true,
						AllowEmpty:         true,
						AllowExplicitNull:  true,
						ConvertNullToEmpty: true,
						Default:            map[string]interface{}{},
					},
				},
				{
					StructField: "Env",
					StringMapValidation: &cr.StringMapValidation{
						Default:    map[string]string{},
						AllowEmpty: true,
					},
				},
				{
					StructField:         "SignatureKey",
					StringPtrValidation: &cr.StringPtrValidation{},
				},
				multiModelValidation(),
			},
		},
	}
}

func monitoringValidation() *cr.StructFieldValidation {
	return &cr.StructFieldValidation{
		StructField: "Monitoring",
		StructValidation: &cr.StructValidation{
			DefaultNil:        true,
			AllowExplicitNull: true,
			StructFieldValidations: []*cr.StructFieldValidation{
				{
					StructField:         "Key",
					StringPtrValidation: &cr.StringPtrValidation{},
				},
				{
					StructField: "ModelType",
					StringValidation: &cr.StringValidation{
						Required:      false,
						AllowEmpty:    true,
						AllowedValues: userconfig.ModelTypeStrings(),
					},
					Parser: func(str string) (interface{}, error) {
						return userconfig.ModelTypeFromString(str), nil
					},
				},
			},
		},
	}
}

func networkingValidation() *cr.StructFieldValidation {
	return &cr.StructFieldValidation{
		StructField: "Networking",
		StructValidation: &cr.StructValidation{
			StructFieldValidations: []*cr.StructFieldValidation{
				{
					StructField: "Endpoint",
					StringPtrValidation: &cr.StringPtrValidation{
						Validator: urls.ValidateEndpoint,
						MaxLength: 1000, // no particular reason other than it works
					},
				},
				{
					StructField: "LocalPort",
					IntPtrValidation: &cr.IntPtrValidation{
						GreaterThan:       pointer.Int(0),
						LessThanOrEqualTo: pointer.Int(math.MaxUint16),
					},
				},
				{
					StructField: "APIGateway",
					StringValidation: &cr.StringValidation{
						AllowedValues: userconfig.APIGatewayTypeStrings(),
						Default:       userconfig.PublicAPIGatewayType.String(),
					},
					Parser: func(str string) (interface{}, error) {
						return userconfig.APIGatewayTypeFromString(str), nil
					},
				},
			},
		},
	}
}

func computeValidation(provider types.ProviderType) *cr.StructFieldValidation {
	cpuDefault := pointer.String("200m")
	if provider == types.LocalProviderType {
		cpuDefault = nil
	}
	return &cr.StructFieldValidation{
		StructField: "Compute",
		StructValidation: &cr.StructValidation{
			StructFieldValidations: []*cr.StructFieldValidation{
				{
					StructField: "CPU",
					StringPtrValidation: &cr.StringPtrValidation{
						Default:           cpuDefault,
						AllowExplicitNull: true,
						CastNumeric:       true,
					},
					Parser: k8s.QuantityParser(&k8s.QuantityValidation{
						GreaterThanOrEqualTo: k8s.QuantityPtr(kresource.MustParse("20m")),
					}),
				},
				{
					StructField: "Mem",
					StringPtrValidation: &cr.StringPtrValidation{
						Default:           nil,
						AllowExplicitNull: true,
					},
					Parser: k8s.QuantityParser(&k8s.QuantityValidation{
						GreaterThanOrEqualTo: k8s.QuantityPtr(kresource.MustParse("20Mi")),
					}),
				},
				{
					StructField: "GPU",
					Int64Validation: &cr.Int64Validation{
						Default:              0,
						GreaterThanOrEqualTo: pointer.Int64(0),
					},
				},
				{
					StructField: "Inf",
					Int64Validation: &cr.Int64Validation{
						Default:              0,
						GreaterThanOrEqualTo: pointer.Int64(0),
					},
				},
			},
		},
	}
}

func autoscalingValidation(provider types.ProviderType) *cr.StructFieldValidation {
	defaultNil := provider == types.LocalProviderType
	allowExplicitNull := provider == types.LocalProviderType
	return &cr.StructFieldValidation{
		StructField: "Autoscaling",
		StructValidation: &cr.StructValidation{
			DefaultNil:        defaultNil,
			AllowExplicitNull: allowExplicitNull,
			StructFieldValidations: []*cr.StructFieldValidation{
				{
					StructField: "MinReplicas",
					Int32Validation: &cr.Int32Validation{
						Default:     1,
						GreaterThan: pointer.Int32(0),
					},
				},
				{
					StructField: "MaxReplicas",
					Int32Validation: &cr.Int32Validation{
						Default:     100,
						GreaterThan: pointer.Int32(0),
					},
				},
				{
					StructField:  "InitReplicas",
					DefaultField: "MinReplicas",
					Int32Validation: &cr.Int32Validation{
						GreaterThan: pointer.Int32(0),
					},
				},
				{
					StructField: "TargetReplicaConcurrency",
					Float64PtrValidation: &cr.Float64PtrValidation{
						GreaterThan: pointer.Float64(0),
					},
				},
				{
					StructField: "MaxReplicaConcurrency",
					Int64Validation: &cr.Int64Validation{
						Default:           consts.DefaultMaxReplicaConcurrency,
						GreaterThan:       pointer.Int64(0),
						LessThanOrEqualTo: pointer.Int64(math.MaxUint16),
					},
				},
				{
					StructField: "Window",
					StringValidation: &cr.StringValidation{
						Default: "60s",
					},
					Parser: cr.DurationParser(&cr.DurationValidation{
						GreaterThanOrEqualTo: &AutoscalingTickInterval,
						MultipleOf:           &AutoscalingTickInterval,
					}),
				},
				{
					StructField: "DownscaleStabilizationPeriod",
					StringValidation: &cr.StringValidation{
						Default: "5m",
					},
					Parser: cr.DurationParser(&cr.DurationValidation{
						GreaterThanOrEqualTo: pointer.Duration(libtime.MustParseDuration("0s")),
					}),
				},
				{
					StructField: "UpscaleStabilizationPeriod",
					StringValidation: &cr.StringValidation{
						Default: "1m",
					},
					Parser: cr.DurationParser(&cr.DurationValidation{
						GreaterThanOrEqualTo: pointer.Duration(libtime.MustParseDuration("0s")),
					}),
				},
				{
					StructField: "MaxDownscaleFactor",
					Float64Validation: &cr.Float64Validation{
						Default:              0.75,
						GreaterThanOrEqualTo: pointer.Float64(0),
						LessThan:             pointer.Float64(1),
					},
				},
				{
					StructField: "MaxUpscaleFactor",
					Float64Validation: &cr.Float64Validation{
						Default:     1.5,
						GreaterThan: pointer.Float64(1),
					},
				},
				{
					StructField: "DownscaleTolerance",
					Float64Validation: &cr.Float64Validation{
						Default:              0.05,
						GreaterThanOrEqualTo: pointer.Float64(0),
						LessThan:             pointer.Float64(1),
					},
				},
				{
					StructField: "UpscaleTolerance",
					Float64Validation: &cr.Float64Validation{
						Default:              0.05,
						GreaterThanOrEqualTo: pointer.Float64(0),
					},
				},
			},
		},
	}
}

func updateStrategyValidation(provider types.ProviderType) *cr.StructFieldValidation {
	defaultNil := provider == types.LocalProviderType
	allowExplicitNull := provider == types.LocalProviderType
	return &cr.StructFieldValidation{
		StructField: "UpdateStrategy",
		StructValidation: &cr.StructValidation{
			DefaultNil:        defaultNil,
			AllowExplicitNull: allowExplicitNull,
			StructFieldValidations: []*cr.StructFieldValidation{
				{
					StructField: "MaxSurge",
					StringValidation: &cr.StringValidation{
						Default:   "25%",
						CastInt:   true,
						Validator: surgeOrUnavailableValidator,
					},
				},
				{
					StructField: "MaxUnavailable",
					StringValidation: &cr.StringValidation{
						Default:   "25%",
						CastInt:   true,
						Validator: surgeOrUnavailableValidator,
					},
				},
			},
		},
	}
}

func multiModelValidation() *cr.StructFieldValidation {
	return &cr.StructFieldValidation{
		StructField: "Models",
		StructValidation: &cr.StructValidation{
			Required:         false,
			TreatNullAsEmpty: false,
			StructFieldValidations: []*cr.StructFieldValidation{
				multiModelPathsValidation(),
				{
					StructField: "Dir",
					StringPtrValidation: &cr.StringPtrValidation{
						Required: false,
					},
				},
				{
					StructField: "SignatureKey",
					StringPtrValidation: &cr.StringPtrValidation{
						Required: false,
					},
				},
				{
					StructField: "CacheSize",
					Int32PtrValidation: &cr.Int32PtrValidation{
						Required:    false,
						GreaterThan: pointer.Int32(0),
					},
				},
				{
					StructField: "DiskCacheSize",
					Int32PtrValidation: &cr.Int32PtrValidation{
						Required:    false,
						GreaterThan: pointer.Int32(0),
					},
				},
			},
		},
	}
}

func multiModelPathsValidation() *cr.StructFieldValidation {
	return &cr.StructFieldValidation{
		StructField: "Paths",
		StructListValidation: &cr.StructListValidation{
			Required:         false,
			TreatNullAsEmpty: true,
			StructValidation: &cr.StructValidation{
				StructFieldValidations: []*cr.StructFieldValidation{
					{
						StructField: "Name",
						StringValidation: &cr.StringValidation{
							Required:                   true,
							AllowEmpty:                 false,
							DisallowedValues:           []string{consts.SingleModelName},
							AlphaNumericDashUnderscore: true,
						},
					},
					{
						StructField: "ModelPath",
						StringValidation: &cr.StringValidation{
							Required:   true,
							AllowEmpty: false,
						},
					},
					{
						StructField: "SignatureKey",
						StringPtrValidation: &cr.StringPtrValidation{
							Required:   false,
							AllowEmpty: false,
						},
					},
				},
			},
		},
	}
}

func surgeOrUnavailableValidator(str string) (string, error) {
	if strings.HasSuffix(str, "%") {
		parsed, ok := s.ParseInt32(strings.TrimSuffix(str, "%"))
		if !ok {
			return "", ErrorInvalidSurgeOrUnavailable(str)
		}
		if parsed < 0 || parsed > 100 {
			return "", ErrorInvalidSurgeOrUnavailable(str)
		}
	} else {
		parsed, ok := s.ParseInt32(str)
		if !ok {
			return "", ErrorInvalidSurgeOrUnavailable(str)
		}
		if parsed < 0 {
			return "", ErrorInvalidSurgeOrUnavailable(str)
		}
	}

	return str, nil
}

func isMultiModelFieldSet(m *userconfig.MultiModels) bool {
	if len(m.Paths) == 0 && m.Dir == nil && m.CacheSize == nil && m.DiskCacheSize == nil {
		return false
	}
	return true
}

var resourceStructValidation = cr.StructValidation{
	AllowExtraFields:       true,
	StructFieldValidations: resourceStructValidations,
}

func ExtractAPIConfigs(configBytes []byte, provider types.ProviderType, configFileName string) ([]userconfig.API, error) {
	var err error

	configData, err := cr.ReadYAMLBytes(configBytes)
	if err != nil {
		return nil, errors.Wrap(err, configFileName)
	}

	configDataSlice, ok := cast.InterfaceToStrInterfaceMapSlice(configData)
	if !ok {
		return nil, errors.Wrap(ErrorMalformedConfig(), configFileName)
	}

	apis := make([]userconfig.API, len(configDataSlice))
	for i, data := range configDataSlice {
		api := userconfig.API{}
		var resourceStruct userconfig.Resource
		errs := cr.Struct(&resourceStruct, data, &resourceStructValidation)
		if errors.HasError(errs) {
			name, _ := data[userconfig.NameKey].(string)
			kindString, _ := data[userconfig.KindKey].(string)
			kind := userconfig.KindFromString(kindString)
			err = errors.Wrap(errors.FirstError(errs...), userconfig.IdentifyAPI(configFileName, name, kind, i))
			return nil, errors.Append(err, fmt.Sprintf("\n\napi configuration schema can be found here: https://docs.cortex.dev/v/%s/deployments/api-configuration", consts.CortexVersionMinor))
		}

		errs = cr.Struct(&api, data, apiValidation(provider, resourceStruct.Kind))

		if errors.HasError(errs) {
			name, _ := data[userconfig.NameKey].(string)
			kindString, _ := data[userconfig.KindKey].(string)
			kind := userconfig.KindFromString(kindString)
			err = errors.Wrap(errors.FirstError(errs...), userconfig.IdentifyAPI(configFileName, name, kind, i))
			return nil, errors.Append(err, fmt.Sprintf("\n\napi configuration schema can be found here: https://docs.cortex.dev/v/%s/deployments/api-configuration", consts.CortexVersionMinor))
		}
		api.Index = i
		api.FileName = configFileName

		api.ApplyDefaultDockerPaths()

		apis[i] = api
	}

	return apis, nil
}

func ValidateAPI(
	api *userconfig.API,
	models *[]CuratedModelResource,
	projectFiles ProjectFiles,
	providerType types.ProviderType,
	awsClient *aws.Client,
) error {
	if providerType == types.AWSProviderType && api.Networking.Endpoint == nil {
		api.Networking.Endpoint = pointer.String("/" + api.Name)
	}

	if err := validatePredictor(api, models, projectFiles, providerType, awsClient); err != nil {
		return errors.Wrap(err, api.Identify(), userconfig.PredictorKey)
	}

	if api.Autoscaling != nil { // should only be nil for local provider
		if err := validateAutoscaling(api); err != nil {
			return errors.Wrap(err, api.Identify(), userconfig.AutoscalingKey)
		}
	}

	if err := validateCompute(api, providerType); err != nil {
		return errors.Wrap(err, api.Identify(), userconfig.ComputeKey)
	}

	if api.UpdateStrategy != nil { // should only be nil for local provider
		if err := validateUpdateStrategy(api.UpdateStrategy); err != nil {
			return errors.Wrap(err, api.Identify(), userconfig.UpdateStrategyKey)
		}
	}

	return nil
}

func validatePredictor(
	api *userconfig.API,
	models *[]CuratedModelResource,
	projectFiles ProjectFiles,
	providerType types.ProviderType,
	awsClient *aws.Client,
) error {
	predictor := api.Predictor
	hasMultiModels := isMultiModelFieldSet(predictor.Models)
	hasSingleModel := predictor.ModelPath != nil

	if hasMultiModels && hasSingleModel {
		return ErrorConflictingFields(userconfig.ModelPathKey, userconfig.ModelsKey)
	}
	if hasMultiModels {
		if len(predictor.Models.Paths) == 0 && predictor.Models.Dir == nil {
			return errors.Wrap(ErrorSpecifyOneOrTheOther(userconfig.ModelsPathsKey, userconfig.ModelsDirKey), userconfig.ModelsKey)
		}
		if predictor.Models.CacheSize == nil && len(predictor.Models.Paths) > 0 {
			predictor.Models.CacheSize = pointer.Int32(int32(len(predictor.Models.Paths)))
		}
		if predictor.Models.DiskCacheSize == nil && predictor.Models.CacheSize != nil {
			predictor.Models.DiskCacheSize = pointer.Int32(*predictor.Models.CacheSize)
		}
	}

	switch predictor.Type {
	case userconfig.PythonPredictorType:
		if err := validatePythonPredictor(predictor, models, providerType, projectFiles, awsClient); err != nil {
			return err
		}
	case userconfig.TensorFlowPredictorType:
		if err := validateTensorFlowPredictor(api, providerType, projectFiles, awsClient); err != nil {
			return err
		}
		if err := validateDockerImagePath(predictor.TensorFlowServingImage, providerType, awsClient); err != nil {
			return errors.Wrap(err, userconfig.TensorFlowServingImageKey)
		}
	case userconfig.ONNXPredictorType:
		if err := validateONNXPredictor(predictor, providerType, projectFiles, awsClient); err != nil {
			return err
		}
	}

	if hasMultiModels && len(predictor.Models.Paths) == 0 && predictor.Models.Dir != nil {
		if predictor.Models.CacheSize == nil {
			predictor.Models.CacheSize = pointer.Int32(int32(len(*models)))
		}
		if predictor.Models.DiskCacheSize == nil {
			predictor.Models.DiskCacheSize = pointer.Int32(int32(len(*models)))
		}
	}

	if *predictor.Models.CacheSize > *predictor.Models.DiskCacheSize {
		return errors.Wrap(ErrorConfigGreaterThanOtherConfig(userconfig.ModelsCacheSizeKey, *predictor.Models.CacheSize, userconfig.ModelsDiskCacheSizeKey, *predictor.Models.DiskCacheSize), userconfig.ModelsKey)
	}

	if err := validateDockerImagePath(predictor.Image, providerType, awsClient); err != nil {
		return errors.Wrap(err, userconfig.ImageKey)
	}

	for key := range predictor.Env {
		if strings.HasPrefix(key, "CORTEX_") {
			return errors.Wrap(ErrorCortexPrefixedEnvVarNotAllowed(), userconfig.EnvKey, key)
		}
	}

	if !projectFiles.HasFile(predictor.Path) {
		return errors.Wrap(files.ErrorFileDoesNotExist(predictor.Path), userconfig.PathKey)
	}

	if predictor.PythonPath != nil {
		if err := validatePythonPath(predictor, projectFiles); err != nil {
			return errors.Wrap(err, userconfig.PythonPathKey)
		}
	}

	return nil
}

func isModelNameIn(models []*userconfig.ModelResource, modelName string) bool {
	for _, model := range models {
		if model.Name == modelName {
			return true
		}
	}
	return false
}

func retrieveModelsResourcesFromPath(path string, projectFiles ProjectFiles, awsClient *aws.Client) ([]*userconfig.ModelResource, error) {
	models := []*userconfig.ModelResource{}

	if aws.IsValidS3Path(path) {
		awsClientForBucket, err := aws.NewFromClientS3Path(path, awsClient)
		if err != nil {
			return models, err
		}

		if isDir, err := awsClientForBucket.IsS3PathDir(path); err != nil {
			return models, err
		} else if isDir {
			modelPaths, err := awsClientForBucket.GetNLevelsDeepFromS3Path(path, 1, true, nil)
			if err != nil {
				return models, err
			}
			var bucket string
			bucket, _, err = aws.SplitS3Path(path)
			if err != nil {
				return models, err
			}

			for _, modelPath := range modelPaths {
				modelName := strings.Split(filepath.Base(modelPath), ".")[0]
				if !isModelNameIn(models, modelName) {
					models = append(models, &userconfig.ModelResource{
						Name:      modelName,
						ModelPath: aws.S3Path(bucket, modelPath),
					})
				} else {
					return []*userconfig.ModelResource{}, ErrorS3ModelNameDuplicate(path, modelName)
				}
			}
		} else {
			return models, ErrorS3DirNotFound(path)
		}

	} else {
		var err error
		if strings.HasPrefix(path, "~/") {
			path, err = files.EscapeTilde(path)
			if err != nil {
				return models, err
			}
		} else {
			path = files.RelToAbsPath(path, projectFiles.ProjectDir())
		}

		var fi os.FileInfo
		fi, err = os.Stat(path)
		if err != nil {
			return models, ErrorInvalidPath(path)
		}
		if !fi.Mode().IsDir() {
			return models, ErrorInvalidDirPath(path)
		}

		var file *os.File
		file, err = os.Open(path)
		if err != nil {
			return models, err
		}

		var modelObjects []string
		modelObjects, err = file.Readdirnames(0)
		if err != nil {
			return models, err
		}

		for _, modelObject := range modelObjects {
			modelName := strings.Split(modelObject, ".")[0]
			if !isModelNameIn(models, modelName) {
				models = append(models, &userconfig.ModelResource{
					Name:      modelName,
					ModelPath: filepath.Join(path, modelObject),
				})
			} else {
				return []*userconfig.ModelResource{}, ErrorS3ModelNameDuplicate(path, modelName)
			}
		}
	}

	return models, nil
}

func validatePythonPredictor(predictor *userconfig.Predictor, models *[]CuratedModelResource, providerType types.ProviderType, projectFiles ProjectFiles, awsClient *aws.Client) error {
	if predictor.SignatureKey != nil {
		return ErrorFieldNotSupportedByPredictorType(userconfig.SignatureKeyKey, userconfig.PythonPredictorType)
	}

	if predictor.TensorFlowServingImage != "" {
		return ErrorFieldNotSupportedByPredictorType(userconfig.TensorFlowServingImageKey, userconfig.PythonPredictorType)
	}

	var modelResources []*userconfig.ModelResource
	if predictor.ModelPath != nil {
		modelResources = []*userconfig.ModelResource{
			{
				Name:      consts.SingleModelName,
				ModelPath: *predictor.ModelPath,
			},
		}
	}
	if isMultiModelFieldSet(predictor.Models) {
		if len(predictor.Models.Paths) > 0 {
			if err := checkDuplicateModelNames(predictor.Models.Paths); err != nil {
				return errors.Wrap(err, userconfig.ModelsKey)
			}
			modelResources = predictor.Models.Paths
		}
		if predictor.Models.Dir != nil {
			var err error
			modelResources, err = retrieveModelsResourcesFromPath(*predictor.Models.Dir, projectFiles, awsClient)
			if err != nil {
				return errors.Wrap(err, userconfig.ModelsKey, userconfig.ModelsDirKey)
			}
		}
		if predictor.Models.SignatureKey != nil {
			return errors.Wrap(ErrorFieldNotSupportedByPredictorType(userconfig.SignatureKeyKey, userconfig.PythonPredictorType), userconfig.ModelsKey)
		}

		fmt.Println("modelResources")
		for _, mr := range modelResources {
			fmt.Println(*mr)
		}
	}

	// for i := range predictor.Models.Paths {
	// 	if predictor.Models.Paths[i].SignatureKey != nil {
	// 		return errors.Wrap(ErrorFieldNotSupportedByPredictorType(userconfig.SignatureKeyKey, predictor.Type), userconfig.ModelsKey, predictor.Models.Paths[i].Name)
	// 	}
	// 	if err := validatePythonModel(predictor.Models.Paths[i], providerType, projectFiles, awsClient); err != nil {
	// 		if predictor.ModelPath == nil {
	// 			return errors.Wrap(err, userconfig.ModelsKey, predictor.Models.Paths[i].Name)
	// 		}
	// 		return err
	// 	}
	// }

	return nil
}

func validatePythonModel(modelResource *userconfig.ModelResource, providerType types.ProviderType, projectFiles ProjectFiles, awsClient *aws.Client) error {
	modelPath := modelResource.ModelPath

	if strings.HasPrefix(modelPath, "s3://") {
		awsClientForBucket, err := aws.NewFromClientS3Path(modelPath, awsClient)
		if err != nil {
			return errors.Wrap(err, userconfig.ModelPathKey)
		}

		modelPath, err = cr.S3PathValidator(modelPath)
		if err != nil {
			return errors.Wrap(err, userconfig.ModelPathKey)
		}

		if strings.HasSuffix(modelPath, ".zip") {
			if ok, err := awsClientForBucket.IsS3PathFile(modelPath); err != nil || !ok {
				fmt.Println(err)
				return errors.Wrap(ErrorS3FileNotFound(modelPath), userconfig.ModelPathKey)
			}
		} else {
			s3Objects, err := awsClientForBucket.ListS3PathDir(modelPath, true, nil)
			if err != nil {
				return errors.Wrap(ErrorS3DirNotFound(modelPath), userconfig.ModelPathKey)
			}
			fmt.Println(s3Objects)
		}
	}

	return nil
}

func validateTensorFlowPredictor(api *userconfig.API, providerType types.ProviderType, projectFiles ProjectFiles, awsClient *aws.Client) error {
	predictor := api.Predictor

	if predictor.ModelPath == nil && predictor.Models == nil {
		return ErrorMissingModel(predictor.Type)
	}

	if predictor.ModelPath != nil {
		modelResource := &userconfig.ModelResource{
			Name:         consts.SingleModelName,
			ModelPath:    *predictor.ModelPath,
			SignatureKey: predictor.SignatureKey,
		}
		// place the model into predictor.Models for ease of use
		predictor.Models.Paths = []*userconfig.ModelResource{modelResource}
	}

	if err := checkDuplicateModelNames(predictor.Models.Paths); err != nil {
		return errors.Wrap(err, userconfig.ModelsKey)
	}

	for i := range predictor.Models.Paths {
		if err := validateTensorFlowModel(predictor.Models.Paths[i], api, providerType, projectFiles, awsClient); err != nil {
			if predictor.ModelPath == nil {
				return errors.Wrap(err, userconfig.ModelsKey, predictor.Models.Paths[i].Name)
			}
			return err
		}
	}

	return nil
}

func validateTensorFlowModel(modelResource *userconfig.ModelResource, api *userconfig.API, providerType types.ProviderType, projectFiles ProjectFiles, awsClient *aws.Client) error {
	modelPath := modelResource.ModelPath

	if strings.HasPrefix(modelPath, "s3://") {
		awsClientForBucket, err := aws.NewFromClientS3Path(modelPath, awsClient)
		if err != nil {
			return errors.Wrap(err, userconfig.ModelPathKey)
		}

		modelPath, err := cr.S3PathValidator(modelPath)
		if err != nil {
			return errors.Wrap(err, userconfig.ModelPathKey)
		}

		if strings.HasSuffix(modelPath, ".zip") {
			if ok, err := awsClientForBucket.IsS3PathFile(modelPath); err != nil || !ok {
				return errors.Wrap(ErrorS3FileNotFound(modelPath), userconfig.ModelPathKey)
			}
		} else {
			isNeuronExport := api.Compute.Inf > 0
			exportPath, err := getTFServingExportFromS3Path(modelPath, isNeuronExport, awsClientForBucket)
			if err != nil {
				return errors.Wrap(err, userconfig.ModelPathKey)
			}
			if exportPath == "" {
				if isNeuronExport {
					return errors.Wrap(ErrorInvalidNeuronTensorFlowDir(modelPath), userconfig.ModelPathKey)
				}
				return errors.Wrap(ErrorInvalidTensorFlowDir(modelPath), userconfig.ModelPathKey)
			}
			modelResource.ModelPath = exportPath
		}
	} else {
		if providerType == types.AWSProviderType {
			return errors.Wrap(ErrorLocalModelPathNotSupportedByAWSProvider(), modelPath, userconfig.ModelPathKey)
		}

		var err error
		if strings.HasPrefix(modelResource.ModelPath, "~/") {
			modelPath, err = files.EscapeTilde(modelPath)
			if err != nil {
				return err
			}
		} else {
			modelPath = files.RelToAbsPath(modelResource.ModelPath, projectFiles.ProjectDir())
		}
		if strings.HasSuffix(modelPath, ".zip") {
			if err := files.CheckFile(modelPath); err != nil {
				return errors.Wrap(err, userconfig.ModelPathKey)
			}
			modelResource.ModelPath = modelPath
		} else if files.IsDir(modelPath) {
			path, err := GetTFServingExportFromLocalPath(modelPath)
			if err != nil {
				return errors.Wrap(err, userconfig.ModelPathKey)
			} else if path == "" {
				return errors.Wrap(ErrorInvalidTensorFlowDir(modelPath), userconfig.ModelPathKey)
			}
			modelResource.ModelPath = path
		} else {
			return errors.Wrap(ErrorInvalidTensorFlowModelPath(), userconfig.ModelPathKey, modelPath)
		}
	}

	return nil
}

func validateONNXPredictor(predictor *userconfig.Predictor, providerType types.ProviderType, projectFiles ProjectFiles, awsClient *aws.Client) error {
	if predictor.SignatureKey != nil {
		return ErrorFieldNotSupportedByPredictorType(userconfig.SignatureKeyKey, predictor.Type)
	}
	if predictor.ModelPath == nil && predictor.Models == nil {
		return ErrorMissingModel(predictor.Type)
	}

	if predictor.ModelPath != nil {
		modelResource := &userconfig.ModelResource{
			Name:      consts.SingleModelName,
			ModelPath: *predictor.ModelPath,
		}
		// place the model into predictor.Models for ease of use
		predictor.Models.Paths = []*userconfig.ModelResource{modelResource}
	}

	if err := checkDuplicateModelNames(predictor.Models.Paths); err != nil {
		return errors.Wrap(err, userconfig.ModelsKey)
	}

	for i := range predictor.Models.Paths {
		if predictor.Models.Paths[i].SignatureKey != nil {
			return errors.Wrap(ErrorFieldNotSupportedByPredictorType(userconfig.SignatureKeyKey, predictor.Type), userconfig.ModelsKey, predictor.Models.Paths[i].Name)
		}
		if err := validateONNXModel(predictor.Models.Paths[i], providerType, projectFiles, awsClient); err != nil {
			if predictor.ModelPath == nil {
				return errors.Wrap(err, userconfig.ModelsKey, predictor.Models.Paths[i].Name)
			}
			return err
		}
	}

	return nil
}

func validateONNXModel(modelResource *userconfig.ModelResource, providerType types.ProviderType, projectFiles ProjectFiles, awsClient *aws.Client) error {
	modelPath := modelResource.ModelPath
	var err error
	if !strings.HasSuffix(modelPath, ".onnx") {
		return errors.Wrap(ErrorInvalidONNXModelPath(), userconfig.ModelPathKey, modelPath)
	}

	if strings.HasPrefix(modelPath, "s3://") {
		awsClientForBucket, err := aws.NewFromClientS3Path(modelPath, awsClient)
		if err != nil {
			return errors.Wrap(err, userconfig.ModelPathKey)
		}

		modelPath, err := cr.S3PathValidator(modelPath)
		if err != nil {
			return errors.Wrap(err, userconfig.ModelPathKey)
		}

		if ok, err := awsClientForBucket.IsS3PathFile(modelPath); err != nil || !ok {
			return errors.Wrap(ErrorS3FileNotFound(modelPath), userconfig.ModelPathKey)
		}
	} else {
		if providerType == types.AWSProviderType {
			return errors.Wrap(ErrorLocalModelPathNotSupportedByAWSProvider(), modelPath, userconfig.ModelPathKey)
		}

		if strings.HasPrefix(modelResource.ModelPath, "~/") {
			modelPath, err = files.EscapeTilde(modelPath)
			if err != nil {
				return err
			}
		} else {
			modelPath = files.RelToAbsPath(modelResource.ModelPath, projectFiles.ProjectDir())
		}
		if err := files.CheckFile(modelPath); err != nil {
			return errors.Wrap(err, userconfig.ModelPathKey)
		}
		modelResource.ModelPath = modelPath
	}
	return nil
}

func getTFServingExportFromS3Path(path string, isNeuronExport bool, awsClientForBucket *aws.Client) (string, error) {
	if isValidTensorFlowS3Directory(path, awsClientForBucket) {
		return path, nil
	}

	bucket, _, err := aws.SplitS3Path(path)
	if err != nil {
		return "", err
	}

	objects, err := awsClientForBucket.ListS3PathDir(path, false, pointer.Int64(1000))
	if err != nil {
		return "", err
	} else if len(objects) == 0 {
		return "", errors.Wrap(ErrorInvalidTensorFlowModelPath(), path)
	}

	highestVersion := int64(0)
	var highestPath string
	for _, object := range objects {
		if !strings.HasSuffix(*object.Key, "saved_model.pb") {
			continue
		}

		keyParts := strings.Split(*object.Key, "/")
		versionStr := keyParts[len(keyParts)-1]
		version, err := strconv.ParseInt(versionStr, 10, 64)
		if err != nil {
			version = 0
		}

		possiblePath := "s3://" + filepath.Join(bucket, filepath.Join(keyParts[:len(keyParts)-1]...))

		if version >= highestVersion {
			if isNeuronExport && isValidNeuronTensorFlowS3Directory(possiblePath, awsClientForBucket) {
				highestVersion = version
				highestPath = possiblePath
			}
			if !isNeuronExport && isValidTensorFlowS3Directory(possiblePath, awsClientForBucket) {
				highestVersion = version
				highestPath = possiblePath
			}
		}
	}

	return highestPath, nil
}

// isValidTensorFlowS3Directory checks that the path contains a valid S3 directory for TensorFlow models
// Must contain the following structure:
// - 1523423423/ (version prefix, usually a timestamp)
// 		- saved_model.pb
//		- variables/
//			- variables.index
//			- variables.data-00000-of-00001 (there are a variable number of these files)
func isValidTensorFlowS3Directory(path string, awsClientForBucket *aws.Client) bool {
	if valid, err := awsClientForBucket.IsS3PathFile(
		aws.JoinS3Path(path, "saved_model.pb"),
		aws.JoinS3Path(path, "variables/variables.index"),
	); err != nil || !valid {
		return false
	}

	if valid, err := awsClientForBucket.IsS3PathPrefix(
		aws.JoinS3Path(path, "variables/variables.data-00000-of"),
	); err != nil || !valid {
		return false
	}
	return true
}

// isValidNeuronTensorFlowS3Directory checks that the path contains a valid S3 directory for Neuron TensorFlow models
// Must contain the following structure:
// - 1523423423/ (version prefix, usually a timestamp)
// 		- saved_model.pb
func isValidNeuronTensorFlowS3Directory(path string, awsClient *aws.Client) bool {
	if valid, err := awsClient.IsS3PathFile(
		aws.JoinS3Path(path, "saved_model.pb"),
	); err != nil || !valid {
		return false
	}

	return true
}

func GetTFServingExportFromLocalPath(path string) (string, error) {
	if err := files.CheckDir(path); err != nil {
		return "", err
	}
	paths, err := files.ListDirRecursive(path, false, files.IgnoreHiddenFiles, files.IgnoreHiddenFolders)
	if err != nil {
		return "", err
	}

	if len(paths) == 0 {
		return "", ErrorDirIsEmpty(path)
	}

	highestVersion := int64(0)
	var highestPath string

	for _, path := range paths {
		if strings.HasSuffix(path, "saved_model.pb") {
			possiblePath := filepath.Dir(path)

			versionStr := filepath.Base(possiblePath)
			version, err := strconv.ParseInt(versionStr, 10, 64)
			if err != nil {
				version = 0
			}

			validTFDirectory, err := IsValidTensorFlowLocalDirectory(possiblePath)
			if err != nil {
				return "", err
			}
			if version > highestVersion && validTFDirectory {
				highestVersion = version
				highestPath = possiblePath
			}
		}
	}

	return highestPath, nil
}

func IsValidTensorFlowLocalDirectory(path string) (bool, error) {
	paths, err := files.ListDirRecursive(path, true, files.IgnoreHiddenFiles, files.IgnoreHiddenFolders)
	if err != nil {
		return false, err
	}
	pathSet := strset.New(paths...)

	if !(pathSet.Has("saved_model.pb") && pathSet.Has("variables/variables.index")) {
		return false, nil
	}

	for _, path := range paths {
		if strings.HasPrefix(path, "variables/variables.data-00000-of") {
			return true, nil
		}
	}

	return false, nil
}

func validatePythonPath(predictor *userconfig.Predictor, projectFiles ProjectFiles) error {
	if !projectFiles.HasDir(*predictor.PythonPath) {
		return ErrorPythonPathNotFound(*predictor.PythonPath)
	}

	return nil
}

func validateAutoscaling(api *userconfig.API) error {
	autoscaling := api.Autoscaling
	predictor := api.Predictor

	if autoscaling.TargetReplicaConcurrency == nil {
		autoscaling.TargetReplicaConcurrency = pointer.Float64(float64(predictor.ProcessesPerReplica * predictor.ThreadsPerProcess))
	}

	if *autoscaling.TargetReplicaConcurrency > float64(autoscaling.MaxReplicaConcurrency) {
		return ErrorConfigGreaterThanOtherConfig(userconfig.TargetReplicaConcurrencyKey, *autoscaling.TargetReplicaConcurrency, userconfig.MaxReplicaConcurrencyKey, autoscaling.MaxReplicaConcurrency)
	}

	if autoscaling.MinReplicas > autoscaling.MaxReplicas {
		return ErrorMinReplicasGreaterThanMax(autoscaling.MinReplicas, autoscaling.MaxReplicas)
	}

	if autoscaling.InitReplicas > autoscaling.MaxReplicas {
		return ErrorInitReplicasGreaterThanMax(autoscaling.InitReplicas, autoscaling.MaxReplicas)
	}

	if autoscaling.InitReplicas < autoscaling.MinReplicas {
		return ErrorInitReplicasLessThanMin(autoscaling.InitReplicas, autoscaling.MinReplicas)
	}

	if api.Compute.Inf > 0 {
		numNeuronCores := api.Compute.Inf * consts.NeuronCoresPerInf
		processesPerReplica := int64(predictor.ProcessesPerReplica)
		if !libmath.IsDivisibleByInt64(numNeuronCores, processesPerReplica) {
			return ErrorInvalidNumberOfInfProcesses(processesPerReplica, api.Compute.Inf, numNeuronCores)
		}
	}

	return nil
}

func validateCompute(api *userconfig.API, providerType types.ProviderType) error {
	compute := api.Compute

	if compute.Inf > 0 && providerType == types.LocalProviderType {
		return ErrorUnsupportedLocalComputeResource(userconfig.InfKey)
	}

	if compute.Inf > 0 && api.Predictor.Type == userconfig.ONNXPredictorType {
		return ErrorFieldNotSupportedByPredictorType(userconfig.InfKey, api.Predictor.Type)
	}

	if compute.GPU > 0 && compute.Inf > 0 {
		return ErrorComputeResourceConflict(userconfig.GPUKey, userconfig.InfKey)
	}

	if compute.Inf > 1 {
		return ErrorInvalidNumberOfInfs(compute.Inf)
	}

	return nil
}

func validateUpdateStrategy(updateStrategy *userconfig.UpdateStrategy) error {
	if (updateStrategy.MaxSurge == "0" || updateStrategy.MaxSurge == "0%") && (updateStrategy.MaxUnavailable == "0" || updateStrategy.MaxUnavailable == "0%") {
		return ErrorSurgeAndUnavailableBothZero()
	}

	return nil
}

func FindDuplicateNames(apis []userconfig.API) []userconfig.API {
	names := make(map[string][]userconfig.API)

	for _, api := range apis {
		names[api.Name] = append(names[api.Name], api)
	}

	for name := range names {
		if len(names[name]) > 1 {
			return names[name]
		}
	}

	return nil
}

func checkDuplicateModelNames(modelResources []*userconfig.ModelResource) error {
	names := strset.New()

	for _, modelResource := range modelResources {
		if names.Has(modelResource.Name) {
			return ErrorDuplicateModelNames(modelResource.Name)
		}
		names.Add(modelResource.Name)
	}

	return nil
}

func validateDockerImagePath(image string, providerType types.ProviderType, awsClient *aws.Client) error {
	if consts.DefaultImagePathsSet.Has(image) {
		return nil
	}
	if _, err := cr.ValidateImageVersion(image, consts.CortexVersion); err != nil {
		return err
	}

	dockerClient, err := docker.GetDockerClient()
	if err != nil {
		return err
	}

	if providerType == types.LocalProviderType {
		// short circuit if the image is already available locally
		if err := docker.CheckLocalImageAccessible(dockerClient, image); err == nil {
			return nil
		}
	}

	dockerAuth := docker.NoAuth
	if regex.IsValidECRURL(image) {
		if awsClient.IsAnonymous {
			return errors.Wrap(ErrorCannotAccessECRWithAnonymousAWSCreds(), image)
		}

		ecrRegion := aws.GetRegionFromECRURL(image)
		if ecrRegion != awsClient.Region {
			return ErrorRegistryInDifferentRegion(ecrRegion, awsClient.Region)
		}

		operatorID, _, err := awsClient.GetCachedAccountID()
		if err != nil {
			return err
		}
		registryID := aws.GetAccountIDFromECRURL(image)

		if operatorID != registryID {
			return ErrorRegistryAccountIDMismatch(registryID, operatorID)
		}

		dockerAuth, err = docker.AWSAuthConfig(awsClient)
		if err != nil {
			if _, ok := errors.CauseOrSelf(err).(awserr.Error); ok {
				// because the operator's IAM user != instances's IAM role (which is created by eksctl and
				// has access to ECR), if the operator IAM doesn't include ECR access, then this will fail
				// even though the instance IAM role may have access; instead, ignore this error because the
				// instance will have access (this will result in missing the case where the image does not exist)
				return nil
			}

			return err
		}
	}

	if err := docker.CheckImageAccessible(dockerClient, image, dockerAuth); err != nil {
		return err
	}

	return nil
}
