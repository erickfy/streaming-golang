package echoserver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bartventer/gorm-multitenancy/examples/v8/internal/models"
	echomw "github.com/bartventer/gorm-multitenancy/middleware/echo/v8"
	multitenancy "github.com/bartventer/gorm-multitenancy/v8"
	"github.com/bartventer/gorm-multitenancy/v8/pkg/scopes"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

type controller struct {
	db   *multitenancy.DB
	once sync.Once
}

func (c *controller) init(e *echo.Echo) {
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())
	e.Use(echomw.WithTenant(echomw.WithTenantConfig{
		Skipper: func(c echo.Context) bool {
			return strings.HasPrefix(c.Request().URL.Path, "/tenants") // skip tenant routes
		},
	}))

	e.POST("/tenants", c.createTenantHandler)
	e.GET("/tenants/:id", c.getTenantHandler)
	e.DELETE("/tenants/:id", c.deleteTenantHandler)
	e.GET("/books", c.getBooksHandler)
	e.POST("/books", c.createBookHandler)
	e.DELETE("/books/:id", c.deleteBookHandler)
	e.PUT("/books/:id", c.updateBookHandler)
}

func Start(ctx context.Context, db *multitenancy.DB) error {
	cr := &controller{db: db}
	return cr.start(ctx)
}

func (cr *controller) start(ctx context.Context) (err error) {
	cr.once.Do(func() {
		e := echo.New()
		cr.init(e)

		srv := &http.Server{
			Addr:         ":8080",
			Handler:      e,
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 10 * time.Second,
		}

		go func() {
			if serveErr := e.StartServer(srv); serveErr != nil {
				log.Printf("listen: %s\n", serveErr)
				err = serveErr
			}
		}()

		<-ctx.Done()

		ctxShutdown, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		if shutdownErr := e.Shutdown(ctxShutdown); shutdownErr != nil {
			log.Printf("Server forced to shutdown: %v", shutdownErr)
			if err == nil {
				err = shutdownErr
			}
		}

		log.Println("Server exiting")
	})
	return err
}

func TenantFromContext(c echo.Context) (string, error) {
	tenantID, ok := c.Get(echomw.TenantKey.String()).(string)
	if !ok {
		return "", errors.New("no tenant in context")
	}
	return tenantID, nil
}

func (cr *controller) createTenantHandler(c echo.Context) error {
	var body models.CreateTenantBody
	var err error
	if err = c.Bind(&body); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	subdomain, subdomainErr := echomw.ExtractSubdomain(body.DomainURL)
	if subdomainErr != nil {
		return echo.NewHTTPError(http.StatusBadRequest, subdomainErr.Error())
	}
	tenant := &models.Tenant{
		TenantModel: multitenancy.TenantModel{
			DomainURL:  body.DomainURL,
			SchemaName: subdomain,
		},
	}
	if err = cr.db.Create(tenant).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if err = cr.db.MigrateTenantModels(context.Background(), tenant.SchemaName); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	res := &models.TenantResponse{
		ID:        tenant.ID,
		DomainURL: tenant.DomainURL,
	}
	return c.JSON(http.StatusCreated, res)
}

func (cr *controller) getTenantHandler(c echo.Context) error {
	dbName := cr.db.Migrator().CurrentDatabase()
	fmt.Println("Database Name:", dbName)
	tenantID := c.Param("id")
	tenant := &models.TenantResponse{}
	if err := cr.db.Table(models.TableNameTenant).First(tenant, tenantID).Error; err != nil {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}
	return c.JSON(http.StatusOK, tenant)
}

func (cr *controller) deleteTenantHandler(c echo.Context) error {
	tenantID := c.Param("id")
	tenant := &models.Tenant{}
	var err error
	if err = cr.db.First(tenant, tenantID).Error; err != nil {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}
	if err = cr.db.OffboardTenant(context.Background(), tenant.SchemaName); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if err = cr.db.Delete(&models.Tenant{}, tenantID).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.NoContent(http.StatusNoContent)
}

func (cr *controller) getBooksHandler(c echo.Context) error {
	tenantID, err := TenantFromContext(c)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	var books []models.BookResponse
	if err = cr.db.Table(models.TableNameBook).Scopes(scopes.WithTenantSchema(tenantID)).Find(&books).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(http.StatusOK, books)
}

func (cr *controller) createBookHandler(c echo.Context) error {
	tenantID, err := TenantFromContext(c)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	var book models.Book
	if err = c.Bind(&book); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	book.TenantSchema = tenantID
	reset, tenantErr := cr.db.UseTenant(context.Background(), tenantID)
	if tenantErr != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, tenantErr.Error())
	}
	defer reset()
	if err = cr.db.Create(&book).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}

	res := &models.BookResponse{
		ID:   book.ID,
		Name: book.Name,
	}
	return c.JSON(http.StatusCreated, res)
}

func (cr *controller) deleteBookHandler(c echo.Context) error {
	tenantID, err := TenantFromContext(c)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	bookID := c.Param("id")
	var book models.Book
	if err = cr.db.Scopes(scopes.WithTenantSchema(tenantID)).First(&book, bookID).Error; err != nil {
		return echo.NewHTTPError(http.StatusNotFound, err.Error())
	}
	if err = cr.db.Scopes(scopes.WithTenantSchema(tenantID)).Delete(&models.Book{}, bookID).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.NoContent(http.StatusNoContent)
}

func (cr *controller) updateBookHandler(c echo.Context) error {
	tenantID, err := TenantFromContext(c)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	bookID := c.Param("id")
	var body models.UpdateBookBody
	if err = c.Bind(&body); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if body.Name == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "name is required")
	}
	book := &models.Book{}
	reset, tenantErr := cr.db.UseTenant(context.Background(), tenantID)
	if tenantErr != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, tenantErr.Error())
	}
	defer reset()
	if err = cr.db.Model(book).Where("id = ?", bookID).Updates(models.Book{
		Name: body.Name,
	}).Error; err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.NoContent(http.StatusOK)
}
