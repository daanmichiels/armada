//go:generate go run ./generate/main.go

package lookoutv2

import (
	"github.com/go-openapi/loads"
	"github.com/go-openapi/runtime/middleware"
	log "github.com/sirupsen/logrus"

	"github.com/G-Research/armada/internal/common/compress"
	"github.com/G-Research/armada/internal/common/database"
	"github.com/G-Research/armada/internal/common/slices"
	"github.com/G-Research/armada/internal/lookoutv2/configuration"
	"github.com/G-Research/armada/internal/lookoutv2/conversions"
	"github.com/G-Research/armada/internal/lookoutv2/gen/restapi"
	"github.com/G-Research/armada/internal/lookoutv2/gen/restapi/operations"
	"github.com/G-Research/armada/internal/lookoutv2/repository"
)

func Serve(configuration configuration.LookoutV2Configuration) error {
	// load embedded swagger file
	swaggerSpec, err := loads.Analyzed(restapi.SwaggerJSON, "")
	if err != nil {
		return err
	}

	db, err := database.OpenPgxPool(configuration.Postgres)
	if err != nil {
		return err
	}

	getJobsRepo := repository.NewSqlGetJobsRepository(db)
	groupJobsRepo := repository.NewSqlGroupJobsRepository(db)
	decompressor := compress.NewThreadSafeZlibDecompressor()
	getJobSpecRepo := repository.NewSqlGetJobSpecRepository(db, decompressor)

	// create new service API
	api := operations.NewLookoutAPI(swaggerSpec)

	api.GetHealthHandler = operations.GetHealthHandlerFunc(
		func(params operations.GetHealthParams) middleware.Responder {
			return operations.NewGetHealthOK().WithPayload("Health check passed")
		},
	)

	api.GetJobsHandler = operations.GetJobsHandlerFunc(
		func(params operations.GetJobsParams) middleware.Responder {
			filters := slices.Map(params.GetJobsRequest.Filters, conversions.FromSwaggerFilter)
			order := conversions.FromSwaggerOrder(params.GetJobsRequest.Order)
			skip := 0
			if params.GetJobsRequest.Skip != nil {
				skip = int(*params.GetJobsRequest.Skip)
			}
			result, err := getJobsRepo.GetJobs(
				params.HTTPRequest.Context(),
				filters,
				order,
				skip,
				int(params.GetJobsRequest.Take))
			if err != nil {
				return operations.NewGetJobsBadRequest().WithPayload(conversions.ToSwaggerError(err.Error()))
			}
			return operations.NewGetJobsOK().WithPayload(&operations.GetJobsOKBody{
				Count: int64(result.Count),
				Jobs:  slices.Map(result.Jobs, conversions.ToSwaggerJob),
			})
		},
	)

	api.GroupJobsHandler = operations.GroupJobsHandlerFunc(
		func(params operations.GroupJobsParams) middleware.Responder {
			filters := slices.Map(params.GroupJobsRequest.Filters, conversions.FromSwaggerFilter)
			order := conversions.FromSwaggerOrder(params.GroupJobsRequest.Order)
			skip := 0
			if params.GroupJobsRequest.Skip != nil {
				skip = int(*params.GroupJobsRequest.Skip)
			}
			result, err := groupJobsRepo.GroupBy(
				params.HTTPRequest.Context(),
				filters,
				order,
				params.GroupJobsRequest.GroupedField,
				params.GroupJobsRequest.Aggregates,
				skip,
				int(params.GroupJobsRequest.Take))
			if err != nil {
				return operations.NewGroupJobsBadRequest().WithPayload(conversions.ToSwaggerError(err.Error()))
			}
			return operations.NewGroupJobsOK().WithPayload(&operations.GroupJobsOKBody{
				Count:  int64(result.Count),
				Groups: slices.Map(result.Groups, conversions.ToSwaggerGroup),
			})
		},
	)

	api.GetJobSpecHandler = operations.GetJobSpecHandlerFunc(
		func(params operations.GetJobSpecParams) middleware.Responder {
			result, err := getJobSpecRepo.GetJobSpec(params.HTTPRequest.Context(), params.GetJobSpecRequest.JobID)
			if err != nil {
				return operations.NewGetJobSpecBadRequest().WithPayload(conversions.ToSwaggerError(err.Error()))
			}
			return operations.NewGetJobSpecOK().WithPayload(&operations.GetJobSpecOKBody{
				Job: result,
			})
		},
	)

	server := restapi.NewServer(api)
	defer func() {
		shutdownErr := server.Shutdown()
		if shutdownErr != nil {
			log.WithError(shutdownErr).Error("Failed to shut down server")
		}
	}()

	server.Port = configuration.ApiPort
	restapi.SetCorsAllowedOrigins(configuration.CorsAllowedOrigins) // This needs to happen before ConfigureAPI
	server.ConfigureAPI()
	if err := server.Serve(); err != nil {
		return err
	}

	return err
}
