# Records docs/demo.gif for the README with https://github.com/charmbracelet/vhs
#
#   export DATABASE_URL=postgres://postgres:postgres@localhost:5432/hookpost?sslmode=disable
#   vhs docs/demo.tape
Output docs/demo.gif
Set Shell "bash"
Set FontSize 14
Set Width 1200
Set Height 700
Set Theme "Catppuccin Mocha"
Set TypingSpeed 40ms

Hide
Type "cd $(git rev-parse --show-toplevel) && clear" Enter
Show
Type "./docs/demo.sh" Sleep 500ms Enter
Sleep 26s
