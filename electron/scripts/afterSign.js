const { execSync } = require('child_process')
const path = require('path')

module.exports = async function afterSign(context) {
  if (context.electronPlatformName !== 'darwin') return
  const appPath = path.join(
    context.appOutDir,
    `${context.packager.appInfo.productFilename}.app`
  )
  console.log('  • ad-hoc signing', appPath)
  execSync(`codesign --force --deep --sign - "${appPath}"`)
}
