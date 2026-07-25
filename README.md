Building on Windows:

MSYS2 UCRT64

pacman -S mingw-w64-ucrt-x86_64-go

echo 'export GOROOT=/ucrt64/lib/go' >> ~/.bash_profile
echo 'export PATH="/ucrt64/bin:$PATH"' >> ~/.bash_profile
source ~/.bash_profile

Then finally ./build.sh

