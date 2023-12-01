// SPDX-License-Identifier: ice License 1.0

package tokenomics

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	stdlibtime "time"

	"github.com/goccy/go-json"
	"github.com/imroc/req/v3"
	"github.com/pkg/errors"
	"github.com/redis/go-redis/v9"

	"github.com/ice-blockchain/eskimo/users"
	"github.com/ice-blockchain/wintr/connectors/storage/v3"
	"github.com/ice-blockchain/wintr/log"
	"github.com/ice-blockchain/wintr/terror"
	"github.com/ice-blockchain/wintr/time"
)

func init() { //nolint:gochecknoinits // It's the only way to tweak the client.
	req.DefaultClient().SetJsonMarshal(json.Marshal)
	req.DefaultClient().SetJsonUnmarshal(json.Unmarshal)
	req.DefaultClient().GetClient().Timeout = requestDeadline
}

func (r *repository) startKYCConfigJSONSyncer(ctx context.Context) {
	ticker := stdlibtime.NewTicker(stdlibtime.Minute) //nolint:gosec,gomnd // Not an  issue.
	defer ticker.Stop()
	r.cfg.kycConfigJSON = new(atomic.Pointer[kycConfigJSON])
	log.Panic(errors.Wrap(r.syncKYCConfigJSON(ctx), "failed to syncKYCConfigJSON"))

	for {
		select {
		case <-ticker.C:
			reqCtx, cancel := context.WithTimeout(ctx, requestDeadline)
			log.Error(errors.Wrap(r.syncKYCConfigJSON(reqCtx), "failed to syncKYCConfigJSON"))
			cancel()
		case <-ctx.Done():
			return
		}
	}
}

//nolint:funlen,gomnd // .
func (r *repository) syncKYCConfigJSON(ctx context.Context) error {
	if resp, err := req.
		SetContext(ctx).
		SetRetryCount(25).
		SetRetryBackoffInterval(10*stdlibtime.Millisecond, 1*stdlibtime.Second).
		SetRetryHook(func(resp *req.Response, err error) {
			if err != nil {
				log.Error(errors.Wrap(err, "failed to fetch KYCConfigJSON, retrying..."))
			} else {
				log.Error(errors.Errorf("failed to fetch KYCConfigJSON with status code:%v, retrying...", resp.GetStatusCode()))
			}
		}).
		SetRetryCondition(func(resp *req.Response, err error) bool {
			return err != nil || resp.GetStatusCode() != http.StatusOK
		}).
		SetHeader("Accept", "application/json").
		SetHeader("Cache-Control", "no-cache, no-store, must-revalidate").
		SetHeader("Pragma", "no-cache").
		SetHeader("Expires", "0").
		Get(r.cfg.KYC.ConfigJSONURL); err != nil {
		return errors.Wrapf(err, "failed to get fetch `%v`", r.cfg.KYC.ConfigJSONURL)
	} else if data, err2 := resp.ToBytes(); err2 != nil {
		return errors.Wrapf(err2, "failed to read body of `%v`", r.cfg.KYC.ConfigJSONURL)
	} else {
		var kycConfig kycConfigJSON
		if err = json.UnmarshalContext(ctx, data, &kycConfig); err != nil {
			return errors.Wrapf(err, "failed to unmarshal into %#v, data: %v", kycConfig, string(data))
		}
		if !kycConfig.FaceAuth.Enabled && len(kycConfig.FaceAuth.DisabledVersions) == 0 && len(kycConfig.FaceAuth.ForceKYCForUserIds) == 0 && !kycConfig.WebFaceAuth.Enabled {
			if body := string(data); !strings.Contains(body, "face-auth") && !strings.Contains(body, "web-face-auth") {
				return errors.Errorf("there's something wrong with the KYCConfigJSON body: %v", body)
			}
		}
		r.cfg.kycConfigJSON.Swap(&kycConfig)

		return nil
	}
}

func (r *repository) validateKYC(ctx context.Context, state *getCurrentMiningSession, skipKYCSteps []users.KYCStep) error { //nolint:funlen // .
	for _, skipKYCStep := range skipKYCSteps {
		if skipKYCStep == users.FacialRecognitionKYCStep || skipKYCStep == users.LivenessDetectionKYCStep || skipKYCStep == users.NoneKYCStep {
			return errors.Errorf("you can't skip kycStep:%v", skipKYCStep)
		}
	}
	if err := r.overrideKYCStateWithEskimoKYCState(ctx, state.UserID, &state.KYCState, skipKYCSteps); err != nil {
		return errors.Wrapf(err, "failed to overrideKYCStateWithEskimoKYCState for %#v", state)
	}
	if state.KYCStepBlocked == users.FacialRecognitionKYCStep && r.isKYCEnabled(ctx, state.LatestDevice, users.FacialRecognitionKYCStep) {
		return terror.New(ErrMiningDisabled, map[string]any{
			"kycStepBlocked": state.KYCStepBlocked,
		})
	}
	switch state.KYCStepPassed {
	case users.NoneKYCStep:
		var (
			atLeastOneMiningStarted = !state.MiningSessionSoloLastStartedAt.IsNil()
			isAfterFirstWindow      = time.Now().Sub(*r.livenessLoadDistributionStartDate.Time) > r.cfg.KYC.FaceRecognitionDelay
			isReservedForToday      = r.cfg.KYC.FaceRecognitionDelay <= r.cfg.MiningSessionDuration.Max || isAfterFirstWindow || int64((time.Now().Sub(*r.livenessLoadDistributionStartDate.Time)%r.cfg.KYC.FaceRecognitionDelay)/r.cfg.MiningSessionDuration.Max) >= state.ID%int64(r.cfg.KYC.FaceRecognitionDelay/r.cfg.MiningSessionDuration.Max) //nolint:lll // .
		)
		if r.isFaceAuthForced(state.UserID) || (atLeastOneMiningStarted && isReservedForToday && r.isKYCEnabled(ctx, state.LatestDevice, users.FacialRecognitionKYCStep)) {
			return terror.New(ErrKYCRequired, map[string]any{
				"kycSteps": []users.KYCStep{users.FacialRecognitionKYCStep, users.LivenessDetectionKYCStep},
			})
		}
	case users.FacialRecognitionKYCStep:
		if r.isKYCEnabled(ctx, state.LatestDevice, users.LivenessDetectionKYCStep) && state.KYCStepBlocked != users.LivenessDetectionKYCStep {
			return terror.New(ErrKYCRequired, map[string]any{
				"kycSteps": []users.KYCStep{users.LivenessDetectionKYCStep},
			})
		}
	case users.LivenessDetectionKYCStep:
		if err := r.verifyLivenessKYC(ctx, state); err != nil {
			return err
		}
		social1Required := len(*state.KYCStepsLastUpdatedAt) == int(users.Social1KYCStep)-1 ||
			time.Now().Sub(*(*state.KYCStepsLastUpdatedAt)[users.Social1KYCStep-1].Time) >= r.cfg.KYC.Social1Delay

		if !state.MiningSessionSoloLastStartedAt.IsNil() && social1Required && r.isKYCEnabled(ctx, state.LatestDevice, users.Social1KYCStep) {
			return terror.New(ErrKYCRequired, map[string]any{
				"kycSteps": []users.KYCStep{users.Social1KYCStep},
			})
		}
	case users.Social1KYCStep:
		if err := r.verifyLivenessKYC(ctx, state); err != nil {
			return err
		}
		if !state.MiningSessionSoloLastStartedAt.IsNil() && r.isQuizRequired(state) && r.isKYCEnabled(ctx, state.LatestDevice, users.QuizKYCStep) {
			return terror.New(ErrKYCRequired, map[string]any{
				"kycSteps": []users.KYCStep{users.QuizKYCStep},
			})
		}
	case users.QuizKYCStep:
		if err := r.verifyLivenessKYC(ctx, state); err != nil {
			return err
		}
		social2Required := len(*state.KYCStepsLastUpdatedAt) == int(users.Social2KYCStep)-1 ||
			time.Now().Sub(*(*state.KYCStepsLastUpdatedAt)[users.Social2KYCStep-1].Time) >= r.cfg.KYC.Social2Delay

		if !state.MiningSessionSoloLastStartedAt.IsNil() && social2Required && r.isKYCEnabled(ctx, state.LatestDevice, users.Social2KYCStep) {
			return terror.New(ErrKYCRequired, map[string]any{
				"kycSteps": []users.KYCStep{users.Social2KYCStep},
			})
		}
	default:
		if err := r.verifyLivenessKYC(ctx, state); err != nil {
			return err
		}
	}

	return nil
}

func (r *repository) verifyLivenessKYC(ctx context.Context, state *getCurrentMiningSession) error {
	var (
		timeSinceLivenessLastFinished = time.Now().Sub(*(*state.KYCStepsLastUpdatedAt)[users.LivenessDetectionKYCStep-1].Time)
		isAfterDelay                  = timeSinceLivenessLastFinished >= r.cfg.KYC.LivenessDelay
		isNetworkDelayAdjusted        = timeSinceLivenessLastFinished >= r.cfg.MiningSessionDuration.Max
		isReservedForToday            = r.cfg.KYC.LivenessDelay > r.cfg.MiningSessionDuration.Max && int64((time.Now().Sub(*r.livenessLoadDistributionStartDate.Time)%r.cfg.KYC.LivenessDelay)/r.cfg.MiningSessionDuration.Max) == state.ID%int64(r.cfg.KYC.LivenessDelay/r.cfg.MiningSessionDuration.Max) //nolint:lll // .
	)
	if isNetworkDelayAdjusted && (isAfterDelay || isReservedForToday) && r.isKYCEnabled(ctx, state.LatestDevice, users.LivenessDetectionKYCStep) {
		return terror.New(ErrKYCRequired, map[string]any{
			"kycSteps": []users.KYCStep{users.LivenessDetectionKYCStep},
		})
	}

	return nil
}

func (r *repository) isQuizRequired(state *getCurrentMiningSession) bool {
	requireQuiz := len(*state.KYCStepsLastUpdatedAt) == int(users.QuizKYCStep)-1 ||
		time.Now().Sub(*(*state.KYCStepsLastUpdatedAt)[users.QuizKYCStep-1].Time) >= r.cfg.KYC.QuizDelay
	if r.cfg.KYC.RequireQuizOnlyOnSpecificDayOfWeek != nil {
		offset := stdlibtime.Duration(state.UTCOffset) * stdlibtime.Minute
		requireQuiz = (len(*state.KYCStepsLastUpdatedAt) == int(users.QuizKYCStep)-1 ||
			time.Now().Sub(*(*state.KYCStepsLastUpdatedAt)[users.QuizKYCStep-1].Time) >= 2*r.cfg.MiningSessionDuration.Max) &&
			int(time.Now().In(stdlibtime.FixedZone(offset.String(), int(offset.Seconds()))).Weekday()) == *r.cfg.KYC.RequireQuizOnlyOnSpecificDayOfWeek
	}

	return requireQuiz
}

func (r *repository) isKYCEnabled(ctx context.Context, latestDevice string, kycStep users.KYCStep) bool {
	var (
		kycConfig = r.cfg.kycConfigJSON.Load()
		isWeb     = isWebClientType(ctx)
	)

	switch kycStep {
	case users.FacialRecognitionKYCStep, users.LivenessDetectionKYCStep:
		if isWeb && !kycConfig.WebFaceAuth.Enabled {
			return false
		}
		if !isWeb && !kycConfig.FaceAuth.Enabled {
			return false
		}
		if !isWeb && kycConfig.FaceAuth.Enabled && !r.isKycStepEnabledForDevice(users.FacialRecognitionKYCStep, latestDevice) {
			return false
		}
	case users.Social1KYCStep:
		if isWeb && !kycConfig.WebSocial1KYC.Enabled {
			return false
		}
		if !isWeb && !kycConfig.Social1KYC.Enabled {
			return false
		}
		if !isWeb && kycConfig.Social1KYC.Enabled && !r.isKycStepEnabledForDevice(users.Social1KYCStep, latestDevice) {
			return false
		}
	case users.QuizKYCStep:
		if isWeb && !kycConfig.WebQuizKYC.Enabled {
			return false
		}
		if !isWeb && !kycConfig.QuizKYC.Enabled {
			return false
		}
		if !isWeb && kycConfig.QuizKYC.Enabled && !r.isKycStepEnabledForDevice(users.QuizKYCStep, latestDevice) {
			return false
		}
	case users.Social2KYCStep:
		if isWeb && !kycConfig.WebSocial2KYC.Enabled {
			return false
		}
		if !isWeb && !kycConfig.Social2KYC.Enabled {
			return false
		}
		if !isWeb && kycConfig.Social2KYC.Enabled && !r.isKycStepEnabledForDevice(users.Social2KYCStep, latestDevice) {
			return false
		}
	}

	return true
}

func (r *repository) isKycStepEnabledForDevice(kycStep users.KYCStep, device string) bool {
	if device == "" {
		return true
	}
	switch kycStep {
	case users.FacialRecognitionKYCStep, users.LivenessDetectionKYCStep:
		var disableFaceAuthFor []string
		if cfgVal := r.cfg.kycConfigJSON.Load(); cfgVal != nil {
			disableFaceAuthFor = cfgVal.FaceAuth.DisabledVersions
		}
		if len(disableFaceAuthFor) == 0 {
			return true
		}
		for _, disabled := range disableFaceAuthFor {
			if strings.EqualFold(device, disabled) {
				return false
			}
		}
	case users.Social1KYCStep:
		var disableSocial1KYCFor []string
		if cfgVal := r.cfg.kycConfigJSON.Load(); cfgVal != nil {
			disableSocial1KYCFor = cfgVal.Social1KYC.DisabledVersions
		}
		if len(disableSocial1KYCFor) == 0 {
			return true
		}
		for _, disabled := range disableSocial1KYCFor {
			if strings.EqualFold(device, disabled) {
				return false
			}
		}
	case users.QuizKYCStep:
		var disableQuizKYCFor []string
		if cfgVal := r.cfg.kycConfigJSON.Load(); cfgVal != nil {
			disableQuizKYCFor = cfgVal.QuizKYC.DisabledVersions
		}
		if len(disableQuizKYCFor) == 0 {
			return true
		}
		for _, disabled := range disableQuizKYCFor {
			if strings.EqualFold(device, disabled) {
				return false
			}
		}
	case users.Social2KYCStep:
		var disableSocial2KYCFor []string
		if cfgVal := r.cfg.kycConfigJSON.Load(); cfgVal != nil {
			disableSocial2KYCFor = cfgVal.Social2KYC.DisabledVersions
		}
		if len(disableSocial2KYCFor) == 0 {
			return true
		}
		for _, disabled := range disableSocial2KYCFor {
			if strings.EqualFold(device, disabled) {
				return false
			}
		}
	}

	return true
}

func (r *repository) isFaceAuthForced(userID string) bool {
	if userID == "" {
		return false
	}
	var forceKYCForUserIds []string
	if cfgVal := r.cfg.kycConfigJSON.Load(); cfgVal != nil {
		forceKYCForUserIds = cfgVal.FaceAuth.ForceKYCForUserIds
	}
	if len(forceKYCForUserIds) == 0 {
		return false
	}
	for _, uID := range forceKYCForUserIds {
		if strings.EqualFold(userID, strings.TrimSpace(uID)) {
			return true
		}
	}

	return false
}

/*
Because existing users have empty KYCState in dragonfly cuz usersTableSource might not have updated it yet.
And because we might need to reset any kyc steps for the user prior to starting to mine.
So we need to call Eskimo for that, to be sure we have the valid kyc state for the user before starting to mine.
*/
func (r *repository) overrideKYCStateWithEskimoKYCState(ctx context.Context, userID string, state *KYCState, skipKYCSteps []users.KYCStep) error {
	request := req.
		SetContext(ctx).
		SetRetryCount(25).
		SetRetryBackoffInterval(10*stdlibtime.Millisecond, 1*stdlibtime.Second).
		SetRetryHook(func(resp *req.Response, err error) {
			if err != nil {
				log.Error(errors.Wrap(err, "failed to fetch eskimo user's state, retrying..."))
			} else {
				body, bErr := resp.ToString()
				log.Error(errors.Wrapf(bErr, "failed to parse negative response body for eskimo user's state"))
				log.Error(errors.Errorf("failed to fetch eskimo user's state with status code:%v, body:%v, retrying...", resp.GetStatusCode(), body))
			}
		}).
		SetRetryCondition(func(resp *req.Response, err error) bool {
			return err != nil || (resp.GetStatusCode() != http.StatusOK && resp.GetStatusCode() != http.StatusUnauthorized && resp.GetStatusCode() != http.StatusForbidden) //nolint:lll // .
		}).
		AddQueryParam("caller", "freezer-refrigerant").
		SetHeader("Authorization", authorization(ctx)).
		SetHeader("X-Account-Metadata", xAccountMetadata(ctx)).
		SetHeader("Accept", "application/json").
		SetHeader("Cache-Control", "no-cache, no-store, must-revalidate").
		SetHeader("Pragma", "no-cache").
		SetHeader("Expires", "0")
	if len(skipKYCSteps) > 0 {
		skipKYCStepsQParamValues := make([]string, 0, len(skipKYCSteps))
		for _, kycStep := range skipKYCSteps {
			skipKYCStepsQParamValues = append(skipKYCStepsQParamValues, strconv.Itoa(int(kycStep)))
		}
		request = request.AddQueryParams("skipKYCSteps", skipKYCStepsQParamValues...)
	}
	if resp, err := request.Post(fmt.Sprintf("%v/users/%v", r.cfg.KYC.TryResetKYCStepsURL, userID)); err != nil {
		return errors.Wrapf(err, "failed to fetch eskimo user state for userID:%v, skipKYCSteps:%#v", userID, skipKYCSteps)
	} else if statusCode := resp.GetStatusCode(); statusCode != http.StatusOK {
		return errors.Errorf("[%v]failed to fetch eskimo user state for userID:%v, skipKYCSteps:%#v", statusCode, userID, skipKYCSteps)
	} else if data, err2 := resp.ToBytes(); err2 != nil {
		return errors.Wrapf(err2, "failed to read body of eskimo user state request for userID:%v, skipKYCSteps:%#v", userID, skipKYCSteps)
	} else {
		return errors.Wrapf(json.Unmarshal(data, state), "failed to unmarshal into %#v, data: %v, skipKYCSteps:%#v", state, string(data), skipKYCSteps)
	}
}

func mustGetLivenessLoadDistributionStartDate(ctx context.Context, db storage.DB) (livenessLoadDistributionStartDate *time.Time) {
	livenessLoadDistributionStartDateString, err := db.Get(ctx, "liveness_load_distribution_start_date").Result()
	if err != nil && errors.Is(err, redis.Nil) {
		err = nil
	}
	log.Panic(errors.Wrap(err, "failed to get liveness_load_distribution_start_date"))
	if livenessLoadDistributionStartDateString != "" {
		livenessLoadDistributionStartDate = new(time.Time)
		log.Panic(errors.Wrapf(livenessLoadDistributionStartDate.UnmarshalText([]byte(livenessLoadDistributionStartDateString)), "failed to parse liveness_load_distribution_start_date `%v`", livenessLoadDistributionStartDateString)) //nolint:lll // .
		livenessLoadDistributionStartDate = time.New(livenessLoadDistributionStartDate.UTC())

		return
	}
	livenessLoadDistributionStartDate = time.New(time.Now().Truncate(24 * stdlibtime.Hour))
	set, sErr := db.SetNX(ctx, "liveness_load_distribution_start_date", livenessLoadDistributionStartDate, 0).Result()
	log.Panic(errors.Wrap(sErr, "failed to set liveness_load_distribution_start_date"))
	if !set {
		return mustGetLivenessLoadDistributionStartDate(ctx, db)
	}

	return livenessLoadDistributionStartDate
}
