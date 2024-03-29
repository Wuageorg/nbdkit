#!/usr/bin/env wish

wm geometry . 920x720
canvas .c -background white
pack .c -fill both -expand true

# Variables to store parsed values
set torrentname ""
set total_length 0
set piece_length 0
set piece_cnt 0
set redraw 0
set num_columns 0

# Read stdin line by line
while {[gets stdin line] != -1} {
    if {$redraw > 0} {
        set piece_cnt [expr {$total_length / $piece_length}]
        puts "Piece Length = $piece_length"
        puts "Total Length = $total_length"
        puts "Piece Count  = $piece_cnt"

        # Calculate the number of columns required to fit all squares
        set square_size 11
        set num_columns [expr {int(sqrt($piece_cnt))}]
        set num_rows [expr {($piece_cnt + $num_columns - 1) / $num_columns}]

        # Calculate the required window size to fit all squares
        set window_width [expr {$num_columns * $square_size}]
        set window_height [expr {$num_rows * $square_size}]
        # Get the current window size
        set current_geometry [wm geometry .]
        regexp {(\d+)x(\d+)} $current_geometry -> current_width current_height
        # Set the window size if it's different from the current size
        if {$window_width != $current_width || $window_height != $current_height} {
            wm geometry . ${window_width}x${window_height}
            .c configure -width $window_width -height $window_height
        }

        # Create a canvas widget to draw squares representing each piece
        .c configure -width $window_width -height $window_height
        # Clear any previous drawings on the canvas
        .c delete all

        # Draw squares representing each piece
        set x 0
        set y 0
        for {set i 0} {$i <= $piece_cnt} {incr i} {
            set x_pos [expr {$x * $square_size}]
            set y_pos [expr {$y * $square_size}]
            set x2 [expr {$x_pos + $square_size}]
            set y2 [expr {$y_pos + $square_size}]
            .c create rectangle $x_pos $y_pos $x2 $y2 -outline black
            incr x
            if {$x == $num_columns} {
                set x 0
                incr y
            }
        }

        set redraw 0
    }

    if {[regexp {\|ReadAt} $line]} {
        if {$piece_length == 0} {
            continue
        }
        # Parse variables in the form KEY=value
        set variables [regexp -all -inline {\w+=\S+} $line]
        set piece_index [lindex [regexp -inline {piece=(\d+)} $line] 1]
        set lo [lindex [regexp -inline {lo=(\d+)} $line] 1]
        set hi [lindex [regexp -inline {hi=(\d+)} $line] 1]
        set state [lindex [regexp -inline {state=(\d+)} $line] 1]
        set bstate [lindex [regexp -inline {bstate=(\d+)} $line] 1]

        # Set the fill color based on the state
        # violet cache miss
        set fill_color "violet"
        if {$state == 0} {
            # reading from memory
            set fill_color "green"
        }
        if {$state == 1} {
            # reading from memory piece is complete
            set fill_color "blue"
        }
        if {$state == 2} {
            # reading from cache (allegedly)
            set fill_color "orange"
        }
        if {$state == $bstate} {
            # concurrent read and state backoff?
            set fill_color "red"
        }

        # Calculate the coordinates of the corresponding square
        set square_row [expr {$piece_index / $num_columns}]
        set square_col [expr {$piece_index % $num_columns}]
        set x_pos [expr {$square_col * $square_size}]
        set y_pos [expr {$square_row * $square_size}]
        set x2 [expr {$x_pos + $square_size}]
        set y2 [expr {$y_pos + $square_size}]

        # Calculate the coordinates of the rectangle within the square
        set rect_x1 [expr {$x_pos + 1}]
        set rect_x2 [expr {$x2 - 1}]
        set rect_y1 [expr {$y_pos + 1}]
        set rect_y2 [expr {$y2 - 1}]

        set square_inside_sz [expr {$square_size - 2}]
        set lo_mapped [expr {int(($lo / $piece_length) * ($square_inside_sz * $square_inside_sz))}]
        set hi_mapped [expr {int(($hi / $piece_length) * ($square_inside_sz * $square_inside_sz))}]

        # Draw the rectangle within the square
        for {set y $rect_y1} {$y <= $rect_y2} {incr y} {
            set rect_start_x $rect_x1
            set rect_end_x $rect_x2
            set inside_range_start [expr {$rect_x1 + $lo_mapped}]
            set inside_range_end [expr {$rect_x1 + $hi_mapped}]
            if {$inside_range_end < $rect_start_x || $inside_range_start > $rect_end_x} {
                continue
            }
            if {$inside_range_start > $rect_start_x} {
                set rect_start_x $inside_range_start
            }
            if {$inside_range_end < $rect_end_x} {
                set rect_end_x $inside_range_end
            }
            .c create rectangle $rect_start_x $y [expr {$rect_end_x + 1}] [expr {$y + 1}] -fill $fill_color -outline ""
        }
    } elseif {[regexp {debug: Pieces Length (\d+)} $line -> piece_length]} {
        if {$total_length > 0} {
            set redraw 1
        }
    } elseif {[regexp {debug: Total Length (\d+)} $line -> total_length]} {
        if {$piece_length > 0} {
            set redraw 1
        }
    } elseif {[regexp {debug: File (.+)} $line -> filename]} {
        wm title . $filename
    }
}
