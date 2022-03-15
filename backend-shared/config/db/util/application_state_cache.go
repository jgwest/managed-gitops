package util

import (
	"context"
	"fmt"

	"github.com/redhat-appstudio/managed-gitops/backend-shared/config/db"
)

// A wrapper over the ApplicationStateCache entries of the datbase

func NewApplicationStateCache() *ApplicationStateCache {

	res := &ApplicationStateCache{
		channel: make(chan applicationStateCacheRequest),
	}

	go applicationStateCacheLoop(res.channel)

	return res
}

type ApplicationStateCacheMessageType int

const (
	ApplicationStateCacheMessage_Get ApplicationStateCacheMessageType = iota
	ApplicationStateCacheMessage_Create
	ApplicationStateCacheMessage_Update
	ApplicationStateCacheMessage_Delete
)

type ApplicationStateCache struct {
	channel chan applicationStateCacheRequest
}

func (asc *ApplicationStateCache) GetApplicationStateById(ctx context.Context, id string) (db.ApplicationState, error) {

	responseChannel := make(chan applicationStateCacheResponse)

	asc.channel <- applicationStateCacheRequest{
		ctx:             ctx,
		primaryKey:      id,
		msgType:         ApplicationStateCacheMessage_Get,
		responseChannel: responseChannel,
	}

	var response applicationStateCacheResponse

	select {
	case response = <-responseChannel:
	case <-ctx.Done():
		return db.ApplicationState{}, fmt.Errorf("context cancelled in GetApplicationStateById")
	}

	if response.err != nil {
		return db.ApplicationState{}, response.err
	}

	return response.applicationState, nil

}

func (asc *ApplicationStateCache) CreateApplicationState(ctx context.Context, appState db.ApplicationState) error {

	// TODO: STUB: implement me!
	return fmt.Errorf("unimplemented")
}
func (asc *ApplicationStateCache) UpdateApplicationState(ctx context.Context, appState db.ApplicationState) error {

	responseChannel := make(chan applicationStateCacheResponse)

	asc.channel <- applicationStateCacheRequest{
		ctx:                  ctx,
		createOrUpdateObject: appState,
		msgType:              ApplicationStateCacheMessage_Get,
		responseChannel:      responseChannel,
	}

	// TODO: STUB: implement me!
	return fmt.Errorf("unimplemented")
}

func (asc *ApplicationStateCache) DeleteApplicationStateById(ctx context.Context, id string) (int, error) {
	// TODO: STUB: implement me!
	return 0, fmt.Errorf("unimplemented")
}

type applicationStateCacheRequest struct {
	ctx     context.Context
	msgType ApplicationStateCacheMessageType

	createOrUpdateObject db.ApplicationState
	primaryKey           string
	responseChannel      chan applicationStateCacheResponse
}

type applicationStateCacheResponse struct {
	applicationState db.ApplicationState
	err              error

	// TODO: STUB: implement 'valueFromCache'
	// valueFromCache is true if the value that was returned came from the cache, false otherwise.
	valueFromCache bool

	// Delete only: rowsAffectedForDelete contains the number of rows that were affected by the deletion oepration
	rowsAffectedForDelete int
}

func applicationStateCacheLoop(inputChan chan applicationStateCacheRequest) {

	cache := map[string]db.ApplicationState{}

	dbQueries, err := db.NewProductionPostgresDBQueries(false)
	if err != nil {
		// TODO: log this as a severe error and return
		return
	}

	for {

		request := <-inputChan

		if request.msgType == ApplicationStateCacheMessage_Get {
			processGetMessage(dbQueries, request, cache)

			// TODO: stub: implement and call the other process*Message functions

		} else {
			// TODO: log this as a severe error and continue
			fmt.Printf("ERROR: unimplemented message type")
			// TODO: STUB - implement me!
			continue
		}

	}
}

func processCreateMessage() {
	// TODO: Stub: similar to update.
}

func processUpdateMessage(dbQueries db.DatabaseQueries, req applicationStateCacheRequest, cache map[string]db.ApplicationState) {

	err := dbQueries.UpdateApplicationState(req.ctx, &req.createOrUpdateObject)

	if err != nil {
		// Update the cache on success
		var appState db.ApplicationState = req.createOrUpdateObject
		cache[req.primaryKey] = appState
	}

	req.responseChannel <- applicationStateCacheResponse{
		err: err,
	}

}

func processDeleteMessage(dbQueries db.DatabaseQueries, req applicationStateCacheRequest, cache map[string]db.ApplicationState) {

	// TODO: Sanity test that req.primaryKey is non-empty, log as severe if it is empty

	// Remove from cache
	delete(cache, req.primaryKey)

	// Remove from DB
	rowsAffected, err := dbQueries.DeleteApplicationStateById(req.ctx, req.primaryKey)

	req.responseChannel <- applicationStateCacheResponse{
		rowsAffectedForDelete: rowsAffected,
		err:                   err,
	}

}

func processGetMessage(dbQueries db.DatabaseQueries, req applicationStateCacheRequest, cache map[string]db.ApplicationState) {

	appState := db.ApplicationState{
		Applicationstate_application_id: req.primaryKey,
	}

	// TODO: Sanity test that req.primaryKey is non-empty

	var err error

	res, exists := cache[appState.Applicationstate_application_id]

	if !exists {
		// If it's not in the cache, then get it from the database
		err = dbQueries.GetApplicationStateById(req.ctx, &appState)
		if err != nil {
			appState = db.ApplicationState{}
		}

		// Update the cache if we get a result from the database
		if err == nil && appState.Applicationstate_application_id != "" {
			cache[appState.Applicationstate_application_id] = appState
		}

	} else {
		// If it is in the cache, return it from the cache
		appState = res
	}

	req.responseChannel <- applicationStateCacheResponse{
		applicationState: res,
		err:              err,
	}

}
