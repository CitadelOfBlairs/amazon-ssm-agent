// Copyright 2016 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may not
// use this file except in compliance with the License. A copy of the
// License is located at
//
// http://aws.amazon.com/apache2.0/
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

// Package configurepackage implements the ConfigurePackage plugin.
package configurepackage

import (
	"fmt"
	"strings"

	"github.com/aws/amazon-ssm-agent/agent/context"
	"github.com/aws/amazon-ssm-agent/agent/contracts"
	"github.com/aws/amazon-ssm-agent/agent/plugins/configurepackage/installer"
	"github.com/aws/amazon-ssm-agent/agent/plugins/configurepackage/localpackages"
	"github.com/aws/amazon-ssm-agent/agent/plugins/configurepackage/trace"
)

// TODO: consider passing in the timeout and cancel channels - does cancel trigger rollback?
// executeConfigurePackage performs install, update and uninstall actions, with rollback support and recovery after reboots
func executeConfigurePackage(
	tracer trace.Tracer,
	context context.T,
	repository localpackages.Repository,
	inst installer.Installer,
	uninst installer.Installer,
	isUpdateInPlace bool,
	initialInstallState localpackages.InstallState,
	output contracts.PluginOutputter) {

	trace := tracer.BeginSection(fmt.Sprintf("execute configure - state: %v", initialInstallState))
	defer trace.End()

	switch initialInstallState {
	case localpackages.Installing, localpackages.Updating:
		// This could be picking up an install after reboot or an update that rebooted during install (after a successful uninstall), or a true update
		executeInstall(tracer, context, repository, inst, uninst, isUpdateInPlace, false, output)
	case localpackages.RollbackInstall:
		executeInstall(tracer, context, repository, uninst, inst, isUpdateInPlace, true, output)
	case localpackages.RollbackUninstall:
		executeUninstall(tracer, context, repository, uninst, inst, isUpdateInPlace, true, output)
	default:
		if uninst != nil && !isUpdateInPlace {
			executeUninstall(tracer, context, repository, inst, uninst, isUpdateInPlace, false, output)
		} else {
			executeInstall(tracer, context, repository, inst, uninst, isUpdateInPlace, false, output)
		}
	}
}

// set package install state and log any error
func setNewInstallState(tracer trace.Tracer, repository localpackages.Repository, inst installer.Installer, newInstallState localpackages.InstallState) {
	trace := tracer.BeginSection(fmt.Sprintf("set install state install %s/%s - state: %v", inst.PackageName(), inst.Version(), newInstallState))

	if err := repository.SetInstallState(tracer, inst.PackageName(), inst.Version(), newInstallState); err != nil {
		trace.WithError(err)
	}

	trace.End()
}

// executeInstall performs install, in-place and legacy update, and validation of a package
func executeInstall(
	tracer trace.Tracer,
	context context.T,
	repository localpackages.Repository,
	inst installer.Installer,
	uninst installer.Installer,
	isUpdateInPlace bool,
	isRollback bool,
	output contracts.PluginOutputter) {

	installtrace := tracer.BeginSection(fmt.Sprintf("install %s/%s. rollback: %t; in-place update: %t", inst.PackageName(), inst.Version(), isRollback, isUpdateInPlace))
	defer installtrace.End()

	var result contracts.PluginOutputter

	if isRollback {
		setNewInstallState(tracer, repository, inst, localpackages.RollbackInstall)
		result = inst.Install(tracer, context)
	} else if isUpdateInPlace {
		setNewInstallState(tracer, repository, inst, localpackages.Updating)
		result = inst.Update(tracer, context)
	} else {
		setNewInstallState(tracer, repository, inst, localpackages.Installing)
		result = inst.Install(tracer, context)
	}

	installtrace.WithExitcode(int64(result.GetExitCode()))

	if result.GetStatus() == contracts.ResultStatusSuccess {
		validatetrace := tracer.BeginSection(fmt.Sprintf("validate %s/%s - rollback: %t", inst.PackageName(), inst.Version(), isRollback))
		result = inst.Validate(tracer, context)
		validatetrace.WithExitcode(int64(result.GetExitCode()))
	}
	if result.GetStatus().IsReboot() {
		tracer.BeginSection(fmt.Sprintf("Rebooting to finish installation of %v %v - rollback: %t", inst.PackageName(), inst.Version(), isRollback))
		output.MarkAsSuccessWithReboot()
		return
	}
	if !result.GetStatus().IsSuccess() {
		// If the execution fails because update script is not present for in-place update, do not roll back.
		// It's not ideal to rely on the error message, but it's uneasy to separate this "validation" error from actual execution error.
		// Ideally when we have a standard that can differentiate error types based on status code or a new status (eg ValidationError),
		// we will refactor to make use of that approach.
		if isUpdateInPlace && strings.Contains(output.GetStderr(), "missing update script") {
			setNewInstallState(tracer, repository, inst, localpackages.Installed)
			output.MarkAsFailed(nil, nil)
			return
		}

		installtrace.AppendErrorf("Failed to install package; install status %v", result.GetStatus())
		if isRollback || uninst == nil {
			// Rollback failed. Mark as failed.
			output.MarkAsFailed(nil, nil)
			// TODO: Remove from repository if this isn't the last successfully installed version?  Run uninstall to clean up?
			setNewInstallState(tracer, repository, inst, localpackages.Failed)
			return
		}
		// Execute rollback
		executeUninstall(tracer, context, repository, uninst, inst, isUpdateInPlace, true, output)
		return
	}
	if uninst != nil {
		// Cleanup after uninstall
		cleanupAfterUninstall(tracer, repository, uninst, output)
	}
	if isRollback {
		installtrace.AppendInfof("Failed to install %v %v, successfully rolled back to %v %v", uninst.PackageName(), uninst.Version(), inst.PackageName(), inst.Version())
		setNewInstallState(tracer, repository, inst, localpackages.Installed)
		output.MarkAsFailed(nil, nil)
		return
	}
	installtrace.AppendInfof("Successfully installed %v %v", inst.PackageName(), inst.Version())
	setNewInstallState(tracer, repository, inst, localpackages.Installed)
	output.MarkAsSucceeded()
	return
}

// executeUninstall performs uninstall of a package
func executeUninstall(
	tracer trace.Tracer,
	context context.T,
	repository localpackages.Repository,
	inst installer.Installer,
	uninst installer.Installer,
	isUpdateInPlace bool,
	isRollback bool,
	output contracts.PluginOutputter) {

	installtrace := tracer.BeginSection(fmt.Sprintf("uninstall %s/%s - rollback: %t", uninst.PackageName(), uninst.Version(), isRollback))
	defer installtrace.End()

	if isRollback {
		setNewInstallState(tracer, repository, uninst, localpackages.RollbackUninstall)
	} else {
		if inst != nil {
			// Setting to Upgrading state means this is the legacy update
			// that requires uninstalling installed version and installing the requested version
			setNewInstallState(tracer, repository, uninst, localpackages.Upgrading)
		} else {
			setNewInstallState(tracer, repository, uninst, localpackages.Uninstalling)
		}
	}

	result := uninst.Uninstall(tracer, context)
	installtrace.WithExitcode(int64(result.GetExitCode()))

	if !result.GetStatus().IsSuccess() {
		installtrace.AppendErrorf("Failed to uninstall version %v of package; uninstall status %v", uninst.Version(), result.GetStatus())
		if inst != nil {
			// Uninstall fails upon rollback. Directly try to reinstall previously installed version.
			executeInstall(tracer, context, repository, inst, uninst, isUpdateInPlace, isRollback, output)
			return
		}
		setNewInstallState(tracer, repository, uninst, localpackages.Failed)
		output.MarkAsFailed(nil, nil)
		return
	}
	if result.GetStatus().IsReboot() {
		tracer.BeginSection(fmt.Sprintf("Rebooting to finish uninstall of %v %v - rollback: %t", uninst.PackageName(), uninst.Version(), isRollback))
		output.MarkAsSuccessWithReboot()
		return
	}
	installtrace.AppendInfof("Successfully uninstalled %v %v", uninst.PackageName(), uninst.Version())
	if inst != nil {
		// Uninstall succeeds upon rollback. Continue to reinstall previously installed version.
		executeInstall(tracer, context, repository, inst, uninst, isUpdateInPlace, isRollback, output)
		return
	}
	cleanupAfterUninstall(tracer, repository, uninst, output)
	setNewInstallState(tracer, repository, uninst, localpackages.None)
	output.MarkAsSucceeded()
}

// cleanupAfterUninstall removes packages that are no longer needed in the repository
func cleanupAfterUninstall(tracer trace.Tracer, repository localpackages.Repository, uninst installer.Installer, output contracts.PluginOutputter) {
	trace := tracer.BeginSection(fmt.Sprintf("cleanup %s/%s", uninst.PackageName(), uninst.Version()))

	if err := repository.RemovePackage(tracer, uninst.PackageName(), uninst.Version()); err != nil {
		trace.WithError(err)
	}

	trace.End()
}
