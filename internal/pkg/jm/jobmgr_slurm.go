// Copyright (c) 2019, Sylabs Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE.md file distributed with the sources of this project regarding your
// rights to use or distribute this software.

package jm

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sylabs/singularity-mpi/internal/pkg/mpi"
	"github.com/sylabs/singularity-mpi/internal/pkg/sympierr"

	"github.com/sylabs/singularity-mpi/internal/pkg/kv"
	"github.com/sylabs/singularity-mpi/internal/pkg/sys"

	"github.com/sylabs/singularity-mpi/internal/pkg/util/sy"
)

const (
	// SlurmParitionKey is the key to use to retrieve the optinal parition id that
	// can be specified in the tool's configuration file.
	SlurmPartitionKey = "slurm_partition"
)

// LoadSlurm is the function used by our job management framework to figure out if Slurm can be used and
// if so return a JM structure with all the "function pointers" to interact with Slurm through our generic
// API.
func LoadSlurm() (bool, JM) {
	var jm JM

	_, err := exec.LookPath("sbatch")
	if err != nil {
		log.Println("* Slurm not detected")
		return false, jm
	}

	jm.ID = SlurmID
	jm.Set = SlurmSetConfig
	jm.Get = SlurmGetConfig
	jm.Submit = SlurmSubmit

	return true, jm
}

// SlurmGetOutput reads the content of the Slurm output file that is associated to a job
func SlurmGetOutput(j *Job, sysCfg *sys.Config) string {
	outputFile := getJobOutputFilePath(j, sysCfg)
	output, err := ioutil.ReadFile(outputFile)
	if err != nil {
		return ""
	}

	return string(output)
}

// SlurmGetError reads the content of the Slurm error file that is associated to a job
func SlurmGetError(j *Job, sysCfg *sys.Config) string {
	errorFile := getJobErrorFilePath(j, sysCfg)
	errorTxt, err := ioutil.ReadFile(errorFile)
	if err != nil {
		return ""
	}

	return string(errorTxt)
}

// SlurmGetConfig is the Slurm function to get the configuration of the job manager
func SlurmGetConfig() error {
	return nil
}

// SlurmSetConfig is the Slurm function to set the configuration of the job manager
func SlurmSetConfig() error {
	log.Println("* Slurm detected, updating singularity-mpi configuration file")
	configFile := sy.GetPathToSyMPIConfigFile()

	err := sy.ConfigFileUpdateEntry(configFile, sys.SlurmEnabledKey, "true")
	if err != nil {
		return fmt.Errorf("failed to update entry %s in %s: %s", sys.SlurmEnabledKey, configFile, err)
	}
	return nil
}

const (
	slurmScriptCmdPrefix = "#SBATCH"
)

func getJobOutputFilePath(j *Job, sysCfg *sys.Config) string {
	errorFilename := j.ContainerCfg.ContainerName + ".out"
	return filepath.Join(sysCfg.ScratchDir, errorFilename)
}

func getJobErrorFilePath(j *Job, sysCfg *sys.Config) string {
	outputFilename := j.ContainerCfg.ContainerName + ".err"
	return filepath.Join(sysCfg.ScratchDir, outputFilename)
}

func generateJobScript(j *Job, sysCfg *sys.Config, kvs []kv.KV) error {
	// Sanity checks
	if j == nil {
		return fmt.Errorf("undefined job")
	}

	// Some sanity checks
	if j.HostCfg == nil {
		return fmt.Errorf("undefined host configuration")
	}

	if j.HostCfg.InstallDir == "" {
		return fmt.Errorf("undefined host installation directory")
	}

	if sysCfg.ScratchDir == "" {
		return fmt.Errorf("undefined scratch directory")
	}

	if j.AppBin == "" {
		return fmt.Errorf("application binary is undefined")
	}

	// Create the batch script
	err := TempFile(j, sysCfg)
	if err != nil {
		if err == sympierr.ErrFileExists {
			log.Printf("* Script %s already esists, skipping\n", j.BatchScript)
			return nil
		}
		return fmt.Errorf("unable to create temporary file: %s", err)
	}

	scriptText := "#!/bin/bash\n#\n"
	partition := kv.GetValue(kvs, SlurmPartitionKey)
	if partition != "" {
		scriptText += slurmScriptCmdPrefix + " --partition=" + partition + "\n"
	}

	if j.NNodes > 0 {
		scriptText += slurmScriptCmdPrefix + " --nodes=" + strconv.FormatInt(j.NNodes, 10) + "\n"
	}

	if j.NP > 0 {
		scriptText += slurmScriptCmdPrefix + " --ntasks=" + strconv.FormatInt(j.NP, 10) + "\n"
	}

	scriptText += slurmScriptCmdPrefix + " --error=" + getJobErrorFilePath(j, sysCfg) + "\n"
	scriptText += slurmScriptCmdPrefix + " --output=" + getJobOutputFilePath(j, sysCfg) + "\n"

	// Set PATH and LD_LIBRARY_PATH
	scriptText += "\nexport PATH=" + j.HostCfg.InstallDir + "/bin:$PATH\n"
	scriptText += "export LD_LIBRARY_PATH=" + j.HostCfg.InstallDir + "/lib:$LD_LIBRARY_PATH\n\n"

	// Add the mpirun command
	mpirunPath := filepath.Join(j.HostCfg.InstallDir, "bin", "mpirun")
	mpirunArgs, err := mpi.GetMpirunArgs(j.HostCfg, j.ContainerCfg)
	if err != nil {
		return fmt.Errorf("unable to get mpirun arguments: %s", err)
	}
	scriptText += "\n" + mpirunPath + " " + strings.Join(mpirunArgs, " ") + "\n"

	err = ioutil.WriteFile(j.BatchScript, []byte(scriptText), 0644)
	if err != nil {
		return fmt.Errorf("unable to write to file %s: %s", j.BatchScript, err)
	}

	return nil
}

// SlurmSubmit prepares the batch script necessary to start a given job.
//
// Note that a script does not need any specific environment to be submitted
func SlurmSubmit(j *Job, sysCfg *sys.Config) (Launcher, error) {
	var l Launcher
	l.Cmd = "sbatch"
	l.CmdArgs = append(l.CmdArgs, "-W") // We always wait until the submitted job terminates

	// Sanity checks
	if j == nil {
		return l, fmt.Errorf("job is undefined")
	}

	kvs, err := sy.LoadMPIConfigFile()
	if err != nil {
		return l, fmt.Errorf("unable to load configuration: %s", err)
	}

	err = generateJobScript(j, sysCfg, kvs)
	if err != nil {
		return l, fmt.Errorf("unable to generate Slurm script: %s", err)
	}
	l.CmdArgs = append(l.CmdArgs, j.BatchScript)

	j.GetOutput = SlurmGetOutput
	j.GetError = SlurmGetError

	return l, nil
}

// SlurmCleanUp is the clean up function for Slurm
func SlurmCleanUp(ctx context.Context, j Job) error {
	err := j.CleanUp()
	if err != nil {
		return fmt.Errorf("job cleanup failed: %s", err)
	}
	return nil
}