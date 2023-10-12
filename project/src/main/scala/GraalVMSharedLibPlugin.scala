package com.typesafe.sbt
package packager
package graalvmnativeimage

import java.io.ByteArrayInputStream

import sbt._
import sbt.Keys.{mainClass, name, _}
import com.typesafe.sbt.packager.{MappingsHelper, Stager}
import com.typesafe.sbt.packager.Keys._
import com.typesafe.sbt.packager.Compat._
import com.typesafe.sbt.packager.archetypes.JavaAppPackaging
import com.typesafe.sbt.packager.docker.{Cmd, DockerPlugin, Dockerfile, ExecCmd}
import com.typesafe.sbt.packager.universal.UniversalPlugin

/**
  * Plugin to compile ahead-of-time native executables.
  *
  * @example Enable the plugin in the `build.sbt`
  * {{{
  *    enablePlugins(GraalVMNativeImagePlugin)
  * }}}
  */
object GraalVMSharedLibPlugin extends AutoPlugin {

  object autoImport extends GraalVMSharedLibKeysEx {
    val GraalVMSharedLib: Configuration = config("graalvm-shared-lib")
  }

  import autoImport._

  private val GraalVMBaseImage = "ghcr.io/graalvm/graalvm-ce"

  override def requires: Plugins = JavaAppPackaging

  override def projectConfigurations: Seq[Configuration] = Seq(GraalVMSharedLib)

  override lazy val projectSettings: Seq[Setting[_]] = Seq(
    GraalVMSharedLib / target := target.value / "graalvm-shared-lib",
    graalVMSharedLibOptions := Seq.empty,
    graalVMSharedLibGraalVersion := None,
    graalVMSharedLibCommand := (if (scala.util.Properties.isWin) "native-image.cmd" else "native-image"),
    GraalVMSharedLib / resourceDirectory := sourceDirectory.value / "graal",
    GraalVMSharedLib / mainClass:= (Compile / mainClass).value
  ) ++ inConfig(GraalVMSharedLib)(scopedSettings)

  private lazy val scopedSettings = Seq[Setting[_]](
    resourceDirectories := Seq(resourceDirectory.value),
    includeFilter := "*",
    resources := resourceDirectories.value.descendantsExcept(includeFilter.value, excludeFilter.value).get,
    UniversalPlugin.autoImport.containerBuildImage := Def.taskDyn {
      graalVMSharedLibGraalVersion.value match {
        case Some(tag) => generateContainerBuildImage(s"$GraalVMBaseImage:$tag")
        case None      => Def.task(None: Option[String])
      }
    }.value,
    packageBin := {
      val targetDirectory = target.value
      val binaryName = name.value
      val nativeImageCommand = graalVMSharedLibCommand.value
      val classpathJars = scriptClasspathOrdering.value
      val extraOptions = graalVMSharedLibOptions.value
      val streams = Keys.streams.value
      val dockerCommand = DockerPlugin.autoImport.dockerExecCommand.value
      val graalResourceDirectories = resourceDirectories.value
      val graalResources = resources.value

      UniversalPlugin.autoImport.containerBuildImage.value match {
        case None =>
          buildLocal(
            targetDirectory,
            binaryName,
            nativeImageCommand,
            classpathJars.map(_._1),
            extraOptions,
            UnbufferedProcessLogger(streams.log)
          )

        case Some(image) =>
          val resourceMappings = MappingsHelper.relative(graalResources, graalResourceDirectories)

          buildInDockerContainer(
            targetDirectory,
            binaryName,
            classpathJars,
            extraOptions,
            dockerCommand,
            resourceMappings,
            image,
            streams
          )
      }
    }
  )

  private def buildLocal(
    targetDirectory: File,
    binaryName: String,
    nativeImageCommand: String,
    classpathJars: Seq[File],
    extraOptions: Seq[String],
    log: ProcessLogger
  ): File = {
    targetDirectory.mkdirs()
    val command = {
      val nativeImageArguments = {
        val classpath = classpathJars.mkString(java.io.File.pathSeparator)
        val cpArgs =
          if (scala.util.Properties.isWin)
            IO.withTemporaryFile("native-image-classpath", ".txt", keepFile = true) { file =>
              IO.write(file, s"--class-path $classpath")
              Seq(s"@${file.absolutePath}")
            }
          else Seq("--class-path", classpath)
        cpArgs ++ Seq(s"-H:Name=$binaryName") ++ extraOptions ++ Seq("--shared")
      }
      Seq(nativeImageCommand) ++ nativeImageArguments
    }

    sys.process.Process(command, targetDirectory) ! log match {
      case 0 => targetDirectory / binaryName
      case x => sys.error(s"Failed to run $command, exit status: " + x)
    }
  }

  private def buildInDockerContainer(
    targetDirectory: File,
    binaryName: String,
    classpathJars: Seq[(File, String)],
    extraOptions: Seq[String],
    dockerCommand: Seq[String],
    resources: Seq[(File, String)],
    image: String,
    streams: TaskStreams
  ): File = {

    stage(targetDirectory, classpathJars, resources, streams)

    val graalDestDir = "/opt/graalvm"
    val stageDestDir = s"$graalDestDir/stage"
    val resourcesDestDir = s"$stageDestDir/resources"

    val command = dockerCommand ++ Seq(
      "run",
      "--workdir",
      "/opt/graalvm",
      "--rm",
      "-v",
      s"${targetDirectory.getAbsolutePath}:$graalDestDir",
      image,
      "-cp",
      (resourcesDestDir +: classpathJars.map(jar => s"$stageDestDir/" + jar._2)).mkString(":"),
      s"-H:Name=$binaryName"
    ) ++ extraOptions ++ Seq("--shared")

    sys.process.Process(command) ! UnbufferedProcessLogger(streams.log) match {
      case 0 => targetDirectory / binaryName
      case x => sys.error(s"Failed to run $command, exit status: " + x)
    }
  }

  /**
    * This can be used to build a custom build image starting from a custom base image. Can be used like so:
    *
    * ```
    * (containerBuildImage in GraalVMNativeImage) := generateContainerBuildImage("my-docker-hub-username/my-graalvm").value
    * ```
    *
    * The passed in docker image must have GraalVM installed and on the PATH, including the gu utility.
    */
  def generateContainerBuildImage(baseImage: String): Def.Initialize[Task[Option[String]]] =
    Def.task {
      val dockerCommand = (GraalVMSharedLib / DockerPlugin.autoImport.dockerExecCommand).value
      val streams = Keys.streams.value

      val (baseName, tag) = baseImage.split(":", 2) match {
        case Array(n, t) => (n, t)
        case Array(n)    => (n, "latest")
      }

      val imageName = s"${baseName.replace('/', '-')}-native-image:$tag"
      import sys.process._
      if ((dockerCommand ++ Seq("image", "ls", imageName, "--quiet")).!!.trim.isEmpty) {
        streams.log.info(s"Generating new GraalVM native-image image based on $baseImage: $imageName")

        val dockerContent = Dockerfile(
          Cmd("FROM", baseImage),
          Cmd("WORKDIR", "/opt/graalvm"),
          ExecCmd("RUN", "gu", "install", "native-image"),
          ExecCmd("RUN", "sh", "-c", "ln -s /opt/graalvm-ce-*/bin/native-image /usr/local/bin/native-image"),
          ExecCmd("ENTRYPOINT", "native-image")
        ).makeContent

        val command = dockerCommand ++ Seq("build", "-t", imageName, "-")

        val ret = sys.process.Process(command) #<
          new ByteArrayInputStream(dockerContent.getBytes()) !
          DockerPlugin.publishLocalLogger(streams.log)

        if (ret != 0)
          throw new RuntimeException("Nonzero exit value when generating GraalVM container build image: " + ret)

      } else
        streams.log.info(s"Using existing GraalVM native-image image: $imageName")

      Some(imageName)
    }

  private def stage(
    targetDirectory: File,
    classpathJars: Seq[(File, String)],
    resources: Seq[(File, String)],
    streams: TaskStreams
  ): File = {
    val stageDir = targetDirectory / "stage"
    val mappings = classpathJars ++ resources.map {
      case (resource, path) => resource -> s"resources/$path"
    }
    Stager.stage(GraalVMBaseImage)(streams, stageDir, mappings)
  }
}

private case class UnbufferedProcessLogger(logger: Logger) extends ProcessLogger {
  override def out(s: => String): Unit = logger.out(s)

  override def err(s: => String): Unit = logger.err(s)

  override def buffer[T](f: => T): T = f
}