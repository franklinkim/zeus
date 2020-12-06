/*
 *  ZEUS - An Electrifying Build System
 *  Copyright (c) 2017 Philipp Mieden <dreadl0ck [at] protonmail [dot] ch>
 *
 *  This program is free software: you can redistribute it and/or modify
 *  it under the terms of the GNU General Public License as published by
 *  the Free Software Foundation, either version 3 of the License, or
 *  (at your option) any later version.
 *
 *  This program is distributed in the hope that it will be useful,
 *  but WITHOUT ANY WARRANTY; without even the implied warranty of
 *  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 *  GNU General Public License for more details.
 *
 *  You should have received a copy of the GNU General Public License
 *  along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/dreadl0ck/readline"
	"github.com/mgutz/ansi"
	"github.com/sirupsen/logrus"
)

var (
	// ErrInvalidArgumentType means the argument type does not match the expected type
	ErrInvalidArgumentType = errors.New("invalid argument type")

	// ErrInvalidArgumentLabel means the argument label does not match the expected label
	ErrInvalidArgumentLabel = errors.New("invalid argument label")

	// ErrUnsupportedLanguage means the language identifier set on the command is not supported
	ErrUnsupportedLanguage = errors.New("unsupported scripting language")

	// ErrEmptyDependency means the dependencies contain an empty string
	ErrEmptyDependency = errors.New("empty dependency")

	// ErrNoFileExtension means the script does not have a file extension
	ErrNoFileExtension = errors.New("no file extension")
)

// command represents a parsed script in memory
type command struct {

	// the path where the script resides
	path string

	// language identifier
	// set automatically via fileExtension
	language string

	// commandName
	name string

	// arguments for the command
	// mapped labels to commandArg instances
	args map[string]*commandArg

	// short descriptive text
	description string

	// manual text
	help string

	// async means the command will be detached
	async bool

	// completer for interactive shell
	PrefixCompleter *readline.PrefixCompleter

	// buildNumber
	buildNumber bool

	// dependency commands will be executed prior to the command itself
	dependencies []string

	// output file(s) of the command
	// if the file exists the command will not be executed
	outputs []string

	// if the command has been generated by a CommandsFile
	// the script that will be executed goes in here
	exec string
}

func (c *command) AsyncRun(args []string) error {
	go func() {
		err := c.Run(args, false)
		if err != nil {
			Log.WithError(err).Error("failed to run command: " + c.name)
		}
	}()

	time.Sleep(50 * time.Millisecond)
	return nil

}

// Run executes the command
func (c *command) Run(args []string, async bool) error {

	// spawn async commands in a new goroutine
	if async {
		return c.AsyncRun(args)
	}

	// handle dependencies
	err := c.execDependencies()
	if err != nil {
		return errors.New("dependency error: " + err.Error())
	}

	return c.AtomicRun(args, false)
}

func (c *command) AtomicRun(args []string, async bool) error {

	// spawn async commands in a new goroutine
	if async {
		return c.AsyncRun(args)
	}

	var (
		cLog         = Log.WithField("prefix", c.name)
		start        = time.Now()
		stdErrBuffer = &bytes.Buffer{}
	)

	// check outputs
	if len(c.outputs) > 0 {

		var outputMissing bool

		// check if all named outputs exist
		for _, output := range c.outputs {

			_, err := os.Stat(output)
			if err != nil {
				Log.Debug("["+ansi.Red+c.name+cp.Reset+"] output missing: ", output)
				outputMissing = true
			}

			if !outputMissing {
				// all output files / dirs exist, skip command
				s.Lock()
				s.currentCommand++
				l.Println(printPrompt() + "[" + strconv.Itoa(s.currentCommand) + "/" + strconv.Itoa(s.numCommands) + "] skipping " + cp.Prompt + c.name + cp.Reset + " because all named outputs exist")
				s.Unlock()
				return nil
			}
		}
	}

	cLog.WithFields(logrus.Fields{
		"prefix": "exec",
		"args":   args,
	}).Debug(cp.CmdName + c.name + cp.Reset)

	s.Lock()
	s.currentCommand++
	s.Unlock()

	// handle args
	argBuffer, err := c.parseArguments(args)
	if err != nil {
		return err
	}

	// init command
	cmd, script, cleanupFunc, err := c.createCommand(argBuffer)
	if err != nil {
		return err
	}

	// set host shell environment
	cmd.Env = os.Environ()
	for name, value := range g.Vars {
		cmd.Env = append(cmd.Env, "zeus."+name+"="+value)
	}

	// don't wire terminalIO for async jobs
	// they can be attached by using the procs builtin
	if !c.async {
		cmd.Stdout = os.Stdout
		cmd.Stderr = io.MultiWriter(os.Stderr, stdErrBuffer)
		cmd.Stdin = os.Stdin
	}

	// incease build number if set
	if c.buildNumber {
		projectData.Lock()
		projectData.fields.BuildNumber++
		projectData.Unlock()
		projectData.update()
	}

	s.Lock()
	if c.async {
		l.Println(printPrompt() + "[" + strconv.Itoa(s.currentCommand) + "/" + strconv.Itoa(s.numCommands) + "] detaching " + cp.Prompt + c.name + cp.Reset)
	} else {
		l.Println(printPrompt() + "[" + strconv.Itoa(s.currentCommand) + "/" + strconv.Itoa(s.numCommands) + "] executing " + cp.Prompt + c.name + cp.Reset)
	}
	s.Unlock()

	// lets go
	err = cmd.Start()
	if err != nil {
		cLog.WithError(err).Fatal("failed to start command: " + c.name)
	}

	// add to processMap
	var (
		id  = processID(randomString())
		pid = cmd.Process.Pid
	)
	cLog.Debug("PID: ", pid)
	addProcess(id, c.name, cmd.Process, pid)

	// after command has finished running, remove from processMap
	defer deleteProcessByPID(pid)

	// wait for process
	return c.waitForProcess(cmd, cleanupFunc, script, id, pid, start, stdErrBuffer)
}

func (c *command) waitForProcess(cmd *exec.Cmd, cleanupFunc func(), script string, id processID, pid int, start time.Time, stdErrBuffer *bytes.Buffer) error {

	cLog := Log.WithField("prefix", "waitForProcess")

	// wait for command to finish execution
	err := cmd.Wait()
	if err != nil {

		// execute cleanupFunc if there is one
		if cleanupFunc != nil {
			cleanupFunc()
		}

		// when there are no globals
		// read the command script directly
		// and print it with line numbers to stdout for easy debugging
		if script == "" {
			scriptBytes, err := ioutil.ReadFile(c.path)
			if err != nil {
				cLog.WithError(err).Error("failed to read script")
			}
			script = string(scriptBytes)
		}

		// langErr can be ignored
		// if the language would not exist the command would have failed to start
		// because ZEUS would not know which interpreter to use
		lang, _ := c.getLanguage()

		// search for lineErr in stdErrBuffer
		i, lineErr := extractLineNumFromError(stdErrBuffer.String(), lang.ErrLineNumberSymbol)
		if lineErr == ErrNoLineNumberFound {
			i = -1
		} else if lineErr != nil {
			l.Println("failed to retrieve line number in which the error occured:", lineErr)
		} else {
			// some scripting languages return a line number
			// thats one line below the real error line
			if lang.CorrectErrLineNumber {
				i--
			}
		}

		// dump complete script and highlight error
		printScript(script, c.name, i)
		if conf.fields.DumpScriptOnError {
			dumpScript(script, c.language, err, stdErrBuffer.String())
		}

		return err
	}

	if c.async {

		// add to process map PID +1
		cLog.Debug("detached PID: ", pid+1)
		addProcess(id, c.name, nil, pid+1)

		func() {
			for {

				// check if detached process is still alive
				// If sig is 0, then no signal is sent, but error checking is still performed
				// this can be used to check for the existence of a process ID or process group ID
				err := exec.Command("kill", "-0", strconv.Itoa(pid+1)).Run()
				if err != nil {
					Log.Debug("detached process with PID " + strconv.Itoa(pid+1) + " exited")
					deleteProcessByPID(pid + 1)

					// execute cleanupFunc if there is one
					if cleanupFunc != nil {
						cleanupFunc()
					}
					return
				}

				time.Sleep(2 * time.Second)
			}
		}()
	} else {
		s.Lock()
		// print stats
		l.Println(
			printPrompt()+"["+strconv.Itoa(s.currentCommand)+"/"+strconv.Itoa(s.numCommands)+"] finished "+cp.Prompt+c.name+cp.Text+" in"+cp.Prompt,
			time.Now().Sub(start),
			cp.Reset,
		)
		s.Unlock()

		// execute cleanupFunc if there is one
		if cleanupFunc != nil {
			cleanupFunc()
		}
	}

	return nil
}

// collect dependencies for the current command
// iterating recursively on all the subdependencies
func (c *command) getDeepDependencies() (deps []string) {

	for _, dep := range c.dependencies {
		// fields
		depFields := strings.Fields(dep)
		if len(depFields) == 0 {
			continue
		}

		// lookup
		depCmd, err := cmdMap.getCommand(depFields[0])
		if err == nil {
			deps = append(deps, depCmd.getDeepDependencies()...)
		}

		deps = append(deps, dep)
	}

	return stripArrayRight(deps)
}

// execute dependencies for the current command
// if their named outputs do not exist
func (c *command) execDependencies() error {

	for _, depCommand := range c.getDeepDependencies() {

		fields := strings.Fields(depCommand)
		if len(fields) == 0 {
			return ErrEmptyDependency
		}

		// lookup
		dep, err := cmdMap.getCommand(fields[0])
		if err != nil {
			return errors.New("invalid dependency: " + err.Error())
		}

		// check if dependency has outputs defined
		if len(dep.outputs) > 0 {

			var outputMissing bool

			// check if all named outputs exist
			for _, output := range dep.outputs {
				_, err := os.Stat(output)
				if err != nil {
					outputMissing = true
				}
			}

			// no outputs missing
			// next iteration
			if !outputMissing {

				s.Lock()
				s.currentCommand++
				l.Println(printPrompt() + "[" + strconv.Itoa(s.currentCommand) + "/" + strconv.Itoa(s.numCommands) + "] skipping " + cp.Prompt + dep.name + cp.Reset)
				s.Unlock()

				continue
			}
		}

		// execute dependency and pass args
		err = dep.AtomicRun(fields[1:], c.async)
		if err != nil {
			Log.WithError(err).Error("failed to execute " + dep.name)
			return err
		}
	}

	return nil
}

// get the language for the current command
func (c *command) getLanguage() (*Language, error) {

	ls.Lock()
	defer ls.Unlock()

	if lang, ok := ls.items[c.language]; ok {
		return lang, nil
	}

	return nil, ErrUnsupportedLanguage
}

// create an exec.Cmd instance ready for execution
// for the given argument buffer
func (c *command) createCommand(argBuffer string) (cmd *exec.Cmd, script string, cleanupFunc func(), err error) {

	var (
		shellCommand []string
		globalVars   string
		globalFuncs  string
	)

	if c.async {
		shellCommand = append(shellCommand, []string{"screen", "-L", "-S", c.name, "-dm"}...)
	}

	lang, err := c.getLanguage()
	if err != nil {
		return
	}

	var stopOnErr bool
	conf.Lock()
	stopOnErr = conf.fields.StopOnError
	conf.Unlock()

	// add interpreter
	shellCommand = append(shellCommand, lang.Interpreter)

	if stopOnErr && lang.FlagStopOnError != "" {
		shellCommand = append(shellCommand, lang.FlagStopOnError)
	}
	if c.path == "" && lang.FlagEvaluateScript != "" {
		shellCommand = append(shellCommand, lang.FlagEvaluateScript)
	}

	globalVars = generateGlobals(lang)

	// add language specific global code
	code, err := ioutil.ReadFile(zeusDir + "/globals/globals" + lang.FileExtension)
	if err == nil {
		globalFuncs = string(code)
	}

	// check if loaded via CommandsFile
	if c.exec != "" {
		script = lang.Bang + "\n" + globalVars + "\n" + globalFuncs + "\n" + argBuffer + "\n" + c.exec
		if lang.UseTempFile {
			// make sure the .tmp dir exists
			os.MkdirAll(scriptDir+"/.tmp", 0700)
			filename := scriptDir + "/.tmp/" + c.name + "_" + randomString() + lang.FileExtension
			f, err := os.Create(filename)
			if err != nil {
				Log.WithError(err).Error("failed to create tmp dir")
				return nil, "", nil, err
			}
			defer f.Close()
			f.WriteString(script)

			// make temp script executable
			err = os.Chmod(filename, 0700)
			if err != nil {
				Log.Error("failed to make script executable")
				return nil, "", nil, err
			}

			shellCommand = append(shellCommand, filename)

			// remove the generated tempfile
			cleanupFunc = func() {
				os.Remove(filename)
			}
		} else {
			shellCommand = append(shellCommand, script)
		}
	} else {

		// make sure script is executable
		// just in case the user wants to run it manually one day
		err = os.Chmod(c.path, 0700)
		if err != nil {
			Log.Error("failed to make script executable")
			return nil, "", nil, err
		}

		shellCommand = append(shellCommand, c.path)
	}

	// Log.Debug("shellCommand: ", shellCommand)

	cmd = exec.Command(shellCommand[0], shellCommand[1:]...)

	// in debug mode, print the complete script that will be executed
	if conf.fields.Debug {
		printScript(script, c.name, -1)
	}

	return cmd, script, cleanupFunc, nil
}

/*
 *	Utils
 */

// get the default value for a commandArg's type
func getDefaultValue(arg *commandArg) string {
	switch arg.argType {
	case reflect.String:
		return ""
	case reflect.Int:
		return "0"
	case reflect.Bool:
		return "false"
	case reflect.Float64:
		return "0.0"
	default:
		return "unknown type"
	}
}

// walk all scripts in the zeus dir and setup commandMap
func findCommands() {

	var (
		cLog    = Log.WithField("prefix", "findCommands")
		start   = time.Now()
		scripts []string
	)

	// walk zeus directory and initialize scripts
	err := filepath.Walk(scriptDir, func(path string, info os.FileInfo, err error) error {

		if err != nil {
			return err
		}

		// ignore self
		if path != scriptDir {

			// ignore sub directories
			if info.IsDir() {
				return filepath.SkipDir
			}

			scripts = append(scripts, path)
		}

		return nil
	})
	if err != nil {
		cLog.WithError(err).Error("failed to walk script directory")
		return
	}

	// sequential approach
	for _, path := range scripts {
		err = initScript(path)
		if err != nil {
			cLog.WithError(err).Fatal("failed to init script: " + path)
		}
	}

	cmdMap.init(start)
}

// dump command to stdout for debugging
func (c *command) dump() {
	w := 15
	fmt.Println("# ---------------------------------------------------------------------------------------------------------------------- #")
	fmt.Println(pad("#  cmdName", w), cp.CmdName+c.name+cp.Reset)
	fmt.Println("# ---------------------------------------------------------------------------------------------------------------------- #")
	fmt.Println(pad("#  path", w), c.path)
	fmt.Println(pad("#  args", w), getArgumentString(c.args)+cp.Reset)
	fmt.Println(pad("#  description", w), c.description)
	fmt.Println(pad("#  help", w), c.help)
	if len(c.dependencies) > 0 {
		fmt.Println(pad("#  len(dependencies)", w), len(c.dependencies))
		fmt.Println("# ====================================================================================================================== #")
		for i, cmd := range c.dependencies {
			fmt.Println("#  dependencies[" + cp.CmdName + strconv.Itoa(i) + cp.Reset + "]")
			fmt.Println("## command: " + cmd)

			fields := strings.Fields(cmd)
			if len(fields) == 0 {
				Log.WithError(ErrEmptyDependency).Error("empty fields")
				continue
			}

			// lookup
			dep, err := cmdMap.getCommand(fields[0])
			if err != nil {
				continue
			}

			dep.dump()
		}
		fmt.Println("# ====================================================================================================================== #")
	}
	fmt.Println(pad("#  buildNumber", w), c.buildNumber)
	fmt.Println(pad("#  async", w), c.async)
	fmt.Println(pad("#  outputs", w), c.outputs)
	if c.exec != "" {
		fmt.Println(pad("#  exec", w))
		for _, line := range strings.Split(c.exec, "\n") {
			l.Println("#      " + line)
		}
	} else {
		fmt.Println(pad("#  exec", w), "")
	}
}

// intialize a command from a path
func initScript(path string) error {

	var (
		lang string
		ext  = filepath.Ext(path)
		name = strings.TrimSuffix(filepath.Base(path), ext)
	)

	// check if script language is supported
	ls.Lock()
	for name, l := range ls.items {
		if l.FileExtension == ext {
			lang = name
		}
	}
	ls.Unlock()

	if lang == "" {
		return errors.New(path + ": " + ErrUnsupportedLanguage.Error())
	}

	// create command instance
	cmd := &command{
		path:            path,
		name:            name,
		args:            make(map[string]*commandArg, 0),
		description:     "",
		help:            "",
		buildNumber:     false,
		dependencies:    []string{},
		outputs:         []string{},
		exec:            "",
		async:           false,
		PrefixCompleter: readline.PcItem(name),
		language:        lang,
	}

	completer.Lock()
	completer.Children = append(completer.Children, cmd.PrefixCompleter)
	completer.Unlock()

	// add to command map
	cmdMap.Lock()
	cmdMap.items[cmd.name] = cmd
	cmdMap.Unlock()

	Log.WithField("prefix", "initScript").Debug("added " + cp.CmdName + cmd.name + cp.Reset + " to the command map")

	return nil

}
