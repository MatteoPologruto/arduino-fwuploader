/*
  FirmwareUploader
  Copyright (c) 2021 Arduino LLC.  All right reserved.

  This library is free software; you can redistribute it and/or
  modify it under the terms of the GNU Lesser General Public
  License as published by the Free Software Foundation; either
  version 2.1 of the License, or (at your option) any later version.

  This library is distributed in the hope that it will be useful,
  but WITHOUT ANY WARRANTY; without even the implied warranty of
  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the GNU
  Lesser General Public License for more details.

  You should have received a copy of the GNU Lesser General Public
  License along with this library; if not, write to the Free Software
  Foundation, Inc., 51 Franklin St, Fifth Floor, Boston, MA  02110-1301  USA
*/

package firmware

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/arduino/FirmwareUploader/flasher"
	"github.com/arduino/FirmwareUploader/indexes"
	"github.com/arduino/FirmwareUploader/indexes/download"
	"github.com/arduino/FirmwareUploader/indexes/firmwareindex"
	programmer "github.com/arduino/FirmwareUploader/programmers"
	"github.com/arduino/arduino-cli/arduino/serialutils"
	"github.com/arduino/arduino-cli/cli/errorcodes"
	"github.com/arduino/arduino-cli/cli/feedback"
	"github.com/arduino/go-properties-orderedmap"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var (
	fqbn    string
	address string
	module  string
)

// NewCommand created a new `version` command
func NewFlashCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "flash",
		Short: "Flashes firmwares to board.",
		Long:  "Flashes specified module firmware to board at specified address. Module name and version can be omitted to install latest version.",
		Example: "" +
			"  " + os.Args[0] + " flash --fqbn arduino:samd:mkr1000 --address COM10 --module WINC1500@19.5.2\n" +
			"  " + os.Args[0] + " flash -b arduino:samd:mkr1000 -a COM10 -m WINC15000\n" +
			"  " + os.Args[0] + " flash -b arduino:samd:mkr1000 -a COM10\n",
		Args: cobra.NoArgs,
		Run:  run,
	}

	command.Flags().StringVarP(&fqbn, "fqbn", "b", "", "Fully Qualified Board Name, e.g.: arduino:samd:mkr1000, arduino:mbed_nano:nanorp2040connect")
	command.Flags().StringVarP(&address, "address", "a", "", "Upload port, e.g.: COM10, /dev/ttyACM0")
	command.Flags().StringVarP(&module, "module", "m", "", "Firmware module ID, e.g.: WINC1500, NINA")
	return command
}

func run(cmd *cobra.Command, args []string) {
	packageIndex, err := indexes.GetPackageIndex()
	if err != nil {
		feedback.Errorf("Can't load package index: %s", err)
		os.Exit(errorcodes.ErrGeneric)
	}

	firmwareIndex, err := indexes.GetFirmwareIndex()
	if err != nil {
		feedback.Errorf("Can't load firmware index: %s", err)
		os.Exit(errorcodes.ErrGeneric)
	}

	if fqbn == "" {
		feedback.Errorf("Error during firmware flashing: missing board fqbn")
		os.Exit(errorcodes.ErrBadArgument)
	}

	if address == "" {
		feedback.Errorf("Error during firmware flashing: missing board address")
		os.Exit(errorcodes.ErrBadArgument)
	}

	board := firmwareIndex.GetBoard(fqbn)
	if board == nil {
		feedback.Errorf("Can't find board with %s fqbn", fqbn)
		os.Exit(errorcodes.ErrBadArgument)
	}

	// Get module name if not specified
	moduleName := ""
	moduleVersion := ""
	if module == "" {
		moduleName = board.Module
	} else {
		moduleSplit := strings.Split(module, "@")
		if len(moduleSplit) == 2 {
			moduleName = moduleSplit[0]
			moduleVersion = moduleSplit[1]
		} else {
			moduleName = module
		}
	}
	// Normalize module name
	moduleName = strings.ToUpper(moduleName)

	var firmware *firmwareindex.IndexFirmware
	if moduleVersion == "" {
		firmware = board.LatestFirmware
	} else {
		firmware = board.GetFirmware(moduleVersion)
	}
	if firmware == nil {
		feedback.Errorf("Error getting firmware for board: %s", fqbn)
		os.Exit(errorcodes.ErrGeneric)
	}

	firmwareFile, err := download.DownloadFirmware(firmware)
	if err != nil {
		feedback.Errorf("Error downloading firmware from %s: %s", firmware.URL, err)
		os.Exit(errorcodes.ErrGeneric)
	}

	toolRelease := indexes.GetToolRelease(packageIndex, board.Uploader)
	if toolRelease == nil {
		feedback.Errorf("Error getting upload tool %s for board %s", board.Uploader, board.Fqbn)
		os.Exit(errorcodes.ErrGeneric)
	}
	uploadToolDir, err := download.DownloadTool(toolRelease)
	if err != nil {
		feedback.Errorf("Error downloading tool %s: %s", board.Uploader, err)
		os.Exit(errorcodes.ErrGeneric)
	}

	loaderSketchPath, err := download.DownloadLoaderSketch(board.LoaderSketch)
	if err != nil {
		feedback.Errorf("Error downloading loader sketch from %s: %s", board.LoaderSketch.URL, err)
		os.Exit(errorcodes.ErrGeneric)
	}

	loaderSketch := strings.ReplaceAll(loaderSketchPath.String(), loaderSketchPath.Ext(), "")

	uploaderCommand := board.GetUploaderCommand()
	uploaderCommand = strings.ReplaceAll(uploaderCommand, "{tool_dir}", filepath.FromSlash(uploadToolDir.String()))
	uploaderCommand = strings.ReplaceAll(uploaderCommand, "{serial.port.file}", address)
	uploaderCommand = strings.ReplaceAll(uploaderCommand, "{loader.sketch}", loaderSketch)

	commandLine, err := properties.SplitQuotedString(uploaderCommand, "\"", false)
	if err != nil {
		feedback.Errorf(`Error splitting command line "%s": %s`, uploaderCommand, err)
		os.Exit(errorcodes.ErrGeneric)
	}

	// Check if board needs a 1200bps touch for upload
	if board.UploadTouch {
		logrus.Info("Putting board into bootloader mode")
		_, err := serialutils.Reset(address, board.UploadWait, nil)
		if err != nil {
			feedback.Errorf("Error during firmware flashing: missing board address")
			os.Exit(errorcodes.ErrGeneric)
		}
	}

	// Flash loader Sketch
	programmerOut := new(bytes.Buffer)
	programmerErr := new(bytes.Buffer)
	if feedback.GetFormat() == feedback.JSON {
		err = programmer.Flash(commandLine, programmerOut, programmerErr)
	} else {
		err = programmer.Flash(commandLine, os.Stdout, os.Stderr)
	}
	if err != nil {
		feedback.Errorf("Error during firmware flashing: %s", err)
		os.Exit(errorcodes.ErrGeneric)
	}

	// Wait a bit after flashing the loader sketch for the board to become
	// available again.
	time.Sleep(1 * time.Second)

	// Get flasher depending on which module to use
	var f flasher.Flasher
	switch moduleName {
	case "NINA":
		f, err = flasher.NewNinaFlasher(address)
	case "SARA":
		f, err = flasher.NewSaraFlasher(address)
	case "WINC1500":
		f, err = flasher.NewWincFlasher(address)
	default:
		err = fmt.Errorf("unknown module: %s", moduleName)
	}
	if err != nil {
		feedback.Errorf("Error during firmware flashing: %s", err)
		os.Exit(errorcodes.ErrGeneric)
	}
	defer f.Close()

	// now flash the actual firmware
	flasherOut := new(bytes.Buffer)
	flasherErr := new(bytes.Buffer)
	if feedback.GetFormat() == feedback.JSON {
		err = f.FlashFirmware(firmwareFile, flasherOut)
	} else {
		err = f.FlashFirmware(firmwareFile, os.Stdout)
	}
	if err != nil {
		feedback.Errorf("Error during firmware flashing: %s", err)
		flasherErr.Write([]byte(fmt.Sprintf("Error during firmware flashing: %s", err)))
	}

	// Print the results
	feedback.PrintResult(&flasher.FlashResult{
		Programmer: (&flasher.ExecOutput{
			Stdout: programmerOut.String(),
			Stderr: programmerErr.String(),
		}),
		Flasher: (&flasher.ExecOutput{
			Stdout: flasherOut.String(),
			Stderr: flasherErr.String(),
		}),
	})
	// Exit if something went wrong but after printing
	if err != nil {
		os.Exit(errorcodes.ErrGeneric)
	}
}
